//go:build linux && cgo

package gpu

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/cihub/seelog"
)

const (
	// DCGMSocketPath is the Unix socket where GPU metrics are written for the agent to consume.
	DCGMSocketPath = "/var/run/ecs/gpu-metrics.sock"
	// collectionInterval is how often we query DCGM.
	collectionInterval = 30 * time.Second
)

// DCGMMetric is the JSON payload written to the socket per GPU device.
type DCGMMetric struct {
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

// StartDCGMCollector starts a background goroutine that connects to nv-hostengine,
// collects GPU metrics, and writes them to a Unix socket for the ECS Agent.
func StartDCGMCollector(ctx context.Context) {
	go runDCGMCollector(ctx)
}

func runDCGMCollector(ctx context.Context) {
	// Wait a bit for nv-hostengine to be ready.
	time.Sleep(5 * time.Second)

	cleanup, err := dcgm.Init(dcgm.Standalone, "127.0.0.1:5555", "0")
	if err != nil {
		seelog.Warnf("DCGM: failed to connect to nv-hostengine: %v", err)
		return
	}
	defer cleanup()
	seelog.Info("DCGM: connected to nv-hostengine")

	// Set up field group with profiling + basic metrics.
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

	fieldGroup, err := dcgm.FieldGroupCreate("ecs_gpu_metrics", fields)
	if err != nil {
		seelog.Warnf("DCGM: failed to create field group: %v", err)
		return
	}

	err = dcgm.WatchFieldsWithGroupEx(fieldGroup, dcgm.GroupAllGPUs(), int64(collectionInterval/time.Microsecond), 0.0, 1)
	if err != nil {
		seelog.Warnf("DCGM: failed to watch fields: %v", err)
		return
	}
	seelog.Info("DCGM: field watches established")

	gpus, err := dcgm.GetSupportedDevices()
	if err != nil {
		seelog.Warnf("DCGM: failed to get devices: %v", err)
		return
	}
	seelog.Infof("DCGM: monitoring %d GPU(s)", len(gpus))

	// Ensure socket directory exists.
	os.MkdirAll("/var/run/ecs", 0755)
	os.Remove(DCGMSocketPath)

	ticker := time.NewTicker(collectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			seelog.Info("DCGM: collector stopping")
			return
		case <-ticker.C:
			metrics := collectMetrics(gpus, fields)
			writeToSocket(metrics)
		}
	}
}

func collectMetrics(gpus []uint, fields []dcgm.Short) []DCGMMetric {
	now := time.Now().UTC().Format(time.RFC3339)
	var metrics []DCGMMetric

	for _, gpu := range gpus {
		values, err := dcgm.GetLatestValuesForFields(gpu, fields)
		if err != nil {
			seelog.Warnf("DCGM: failed to get values for GPU %d: %v", gpu, err)
			continue
		}

		// Get UUID separately.
		uuidVals, _ := dcgm.GetLatestValuesForFields(gpu, []dcgm.Short{dcgm.DCGM_FI_DEV_UUID})
		uuid := ""
		if len(uuidVals) > 0 {
			uuid = uuidVals[0].String()
		}

		m := DCGMMetric{
			Timestamp: now,
			GPUUUID:   uuid,
		}

		// Map field values by position matching the fields slice.
		if len(values) >= 9 {
			m.GPUUtilization = safeFloat(values[0])
			m.MemoryUtilization = safeFloat(values[1])
			m.MemoryUsedBytes = safeUint(values[2]) * 1024 * 1024
			m.MemoryTotalBytes = safeUint(values[3]) * 1024 * 1024
			m.PowerW = safeFloat(values[4])
			m.TemperatureC = safeFloat(values[5])
			m.SMActive = safeFloat(values[6])
			m.SMOccupancy = safeFloat(values[7])
			m.DRAMActive = safeFloat(values[8])
		}

		metrics = append(metrics, m)
	}
	return metrics
}

func writeToSocket(metrics []DCGMMetric) {
	if len(metrics) == 0 {
		return
	}

	data, err := json.Marshal(metrics)
	if err != nil {
		seelog.Warnf("DCGM: marshal error: %v", err)
		return
	}
	data = append(data, '\n')

	// Write to socket (connect per write, agent listens).
	conn, err := net.DialTimeout("unix", DCGMSocketPath, 2*time.Second)
	if err != nil {
		// If agent isn't listening yet, write to file as fallback.
		os.WriteFile(DCGMSocketPath+".json", data, 0644)
		return
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(data)
}

func safeFloat(fv dcgm.FieldValue_v1) float64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Float64()
	// Check for DCGM sentinel values.
	if v > 140737488355327.0 || v < 0 {
		// Try int64 interpretation (for integer fields like utilization).
		iv := fv.Int64()
		if iv >= 0 && iv < 0x7ffffff0 {
			return float64(iv)
		}
		return 0
	}
	return v
}

func safeUint(fv dcgm.FieldValue_v1) uint64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Int64()
	if v < 0 || v >= 0x7ffffffffffffff0 {
		return 0
	}
	return uint64(v)
}
