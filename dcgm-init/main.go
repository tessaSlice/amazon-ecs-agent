//go:build linux && cgo

// dcgm-init is a standalone daemon that collects GPU metrics from DCGM (nv-hostengine)
// and writes them to a Unix socket for the ECS Agent to consume.
// It runs as a systemd service on GPU instances alongside ecs-init.
package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	log "github.com/cihub/seelog"
)

const (
	socketPath         = "/var/run/ecs/gpu-metrics.sock"
	collectionInterval = 30 * time.Second
)

type GPUMetric struct {
	Timestamp         string  `json:"timestamp"`
	GPUUUID           string  `json:"gpu_uuid"`
	GPUUtilization    float64 `json:"gpu_utilization_pct"`
	MemoryUtilization float64 `json:"memory_utilization_pct"`
	MemoryUsedBytes   uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes  uint64  `json:"memory_total_bytes"`
	PowerW            float64 `json:"power_w"`
	TemperatureC      float64 `json:"temperature_c"`
	SMActive          float64 `json:"sm_active"`
	SMOccupancy       float64 `json:"sm_occupancy"`
	DRAMActive        float64 `json:"dram_active"`
}

func main() {
	log.ReplaceLogger(log.Default)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Connect to nv-hostengine
	cleanup, err := dcgm.Init(dcgm.Standalone, "127.0.0.1:5555", "0")
	if err != nil {
		log.Errorf("dcgm-init: failed to connect to nv-hostengine: %v", err)
		os.Exit(1)
	}
	defer cleanup()
	log.Infof("dcgm-init: connected to nv-hostengine")

	// Watch profiling + basic fields
	fields := []dcgm.Short{
		dcgm.DCGM_FI_DEV_GPU_UTIL,
		dcgm.DCGM_FI_DEV_MEM_COPY_UTIL,
		dcgm.DCGM_FI_DEV_FB_USED,
		dcgm.DCGM_FI_DEV_FB_TOTAL,
		dcgm.DCGM_FI_DEV_POWER_USAGE,
		dcgm.DCGM_FI_DEV_GPU_TEMP,
		dcgm.DCGM_FI_PROF_SM_ACTIVE,
		dcgm.DCGM_FI_PROF_SM_OCCUPANCY,
		dcgm.DCGM_FI_PROF_DRAM_ACTIVE,
	}

	fg, err := dcgm.FieldGroupCreate("dcgm_init", fields)
	if err != nil {
		log.Errorf("dcgm-init: field group error: %v", err)
		os.Exit(1)
	}
	if err := dcgm.WatchFieldsWithGroupEx(fg, dcgm.GroupAllGPUs(), int64(collectionInterval/time.Microsecond), 0.0, 1); err != nil {
		log.Errorf("dcgm-init: watch error: %v", err)
		os.Exit(1)
	}

	gpus, err := dcgm.GetSupportedDevices()
	if err != nil {
		log.Errorf("dcgm-init: get devices error: %v", err)
		os.Exit(1)
	}
	log.Infof("dcgm-init: monitoring %d GPU(s) with profiling metrics", len(gpus))

	// Create socket directory
	os.MkdirAll("/var/run/ecs", 0755)

	// Start listener (agent connects to us)
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Errorf("dcgm-init: socket listen error: %v", err)
		os.Exit(1)
	}
	defer ln.Close()
	os.Chmod(socketPath, 0666)
	log.Infof("dcgm-init: serving on %s", socketPath)

	// Accept connections from agent (non-blocking)
	var clients []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			clients = append(clients, conn)
		}
	}()

	// Collection loop
	ticker := time.NewTicker(collectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sig:
			log.Infof("dcgm-init: shutting down")
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics := collect(gpus, fields)
			broadcast(metrics, &clients)
		}
	}
}

func collect(gpus []uint, fields []dcgm.Short) []GPUMetric {
	var metrics []GPUMetric
	now := time.Now().UTC().Format(time.RFC3339)

	for _, gpu := range gpus {
		values, err := dcgm.GetLatestValuesForFields(gpu, fields)
		if err != nil {
			continue
		}
		uuidVals, _ := dcgm.GetLatestValuesForFields(gpu, []dcgm.Short{dcgm.DCGM_FI_DEV_UUID})
		uuid := ""
		if len(uuidVals) > 0 && uuidVals[0].Status == 0 {
			uuid = uuidVals[0].String()
		}

		m := GPUMetric{Timestamp: now, GPUUUID: uuid}
		if len(values) >= 9 {
			m.GPUUtilization = intFieldToFloat(values[0])
			m.MemoryUtilization = intFieldToFloat(values[1])
			m.MemoryUsedBytes = intFieldToUint(values[2]) * 1024 * 1024
			m.MemoryTotalBytes = intFieldToUint(values[3]) * 1024 * 1024
			m.PowerW = floatField(values[4])
			m.TemperatureC = intFieldToFloat(values[5])
			m.SMActive = floatField(values[6])
			m.SMOccupancy = floatField(values[7])
			m.DRAMActive = floatField(values[8])
		}
		metrics = append(metrics, m)
	}
	if len(metrics) > 0 {
		log.Infof("dcgm-init: collected gpu_util=%.0f%% sm_active=%.6f power=%.1fW temp=%.0fC",
			metrics[0].GPUUtilization, metrics[0].SMActive, metrics[0].PowerW, metrics[0].TemperatureC)
	}
	return metrics
}

func broadcast(metrics []GPUMetric, clients *[]net.Conn) {
	if len(metrics) == 0 {
		return
	}
	data, _ := json.Marshal(metrics)
	data = append(data, '\n')

	var alive []net.Conn
	for _, c := range *clients {
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(data); err != nil {
			c.Close()
		} else {
			alive = append(alive, c)
		}
	}
	*clients = alive

	// Also write latest to a file for agents that connect later
	os.WriteFile(socketPath+".latest", data, 0644)
}

// intFieldToFloat reads an int64 DCGM field and returns as float64.
func intFieldToFloat(fv dcgm.FieldValue_v1) float64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Int64()
	if v < 0 || v >= 0x7ffffff0 {
		return 0
	}
	return float64(v)
}

// intFieldToUint reads an int64 DCGM field and returns as uint64.
func intFieldToUint(fv dcgm.FieldValue_v1) uint64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Int64()
	if v < 0 || v >= 0x7ffffffffffffff0 {
		return 0
	}
	return uint64(v)
}

// floatField reads a float64 DCGM field.
func floatField(fv dcgm.FieldValue_v1) float64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Float64()
	if v > 140737488355327.0 || v < 0 {
		return 0
	}
	return v
}
