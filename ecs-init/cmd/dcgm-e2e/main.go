//go:build linux && cgo

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

const socketPath = "/var/run/ecs/gpu-metrics.sock"

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Load AWS config for CloudWatch
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-west-2"))
	if err != nil {
		fmt.Printf("AWS config error: %v\n", err)
		os.Exit(1)
	}
	cwClient := cloudwatch.NewFromConfig(awsCfg)

	// Start agent listener (receives metrics from collector via socket)
	go agentListener(ctx, cwClient)
	time.Sleep(1 * time.Second)

	// Connect to DCGM (nv-hostengine must be running on localhost:5555)
	cleanup, err := dcgm.Init(dcgm.Standalone, "127.0.0.1:5555", "0")
	if err != nil {
		fmt.Printf("DCGM init failed: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()
	fmt.Println("DCGM: connected to nv-hostengine")

	// Fields: basic + profiling
	fields := []dcgm.Short{
		dcgm.DCGM_FI_DEV_GPU_UTIL,      // 0
		dcgm.DCGM_FI_DEV_MEM_COPY_UTIL, // 1
		dcgm.DCGM_FI_DEV_FB_USED,       // 2
		dcgm.DCGM_FI_DEV_FB_TOTAL,      // 3
		dcgm.DCGM_FI_DEV_POWER_USAGE,   // 4
		dcgm.DCGM_FI_DEV_GPU_TEMP,      // 5
		dcgm.DCGM_FI_PROF_SM_ACTIVE,    // 6
		dcgm.DCGM_FI_PROF_SM_OCCUPANCY, // 7
		dcgm.DCGM_FI_PROF_DRAM_ACTIVE,  // 8
	}

	fg, err := dcgm.FieldGroupCreate("ecs_e2e", fields)
	if err != nil {
		fmt.Printf("Field group error: %v\n", err)
		os.Exit(1)
	}
	if err := dcgm.WatchFieldsWithGroupEx(fg, dcgm.GroupAllGPUs(), 30000000, 0.0, 1); err != nil {
		fmt.Printf("Watch error: %v\n", err)
		os.Exit(1)
	}

	gpus, _ := dcgm.GetSupportedDevices()
	fmt.Printf("DCGM: monitoring %d GPU(s), profiling fields active\n", len(gpus))

	// Collect and send every 30s
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	collectAndSend(gpus, fields)
	for {
		select {
		case <-sig:
			fmt.Println("Shutting down")
			return
		case <-ticker.C:
			collectAndSend(gpus, fields)
		}
	}
}

func collectAndSend(gpus []uint, fields []dcgm.Short) {
	var metrics []GPUMetric
	for _, gpu := range gpus {
		values, err := dcgm.GetLatestValuesForFields(gpu, fields)
		if err != nil {
			fmt.Printf("  GPU %d: field read error: %v\n", gpu, err)
			continue
		}
		uuidVals, _ := dcgm.GetLatestValuesForFields(gpu, []dcgm.Short{dcgm.DCGM_FI_DEV_UUID})
		uuid := ""
		if len(uuidVals) > 0 {
			uuid = uuidVals[0].String()
		}
		m := GPUMetric{Timestamp: time.Now().UTC().Format(time.RFC3339), GPUUUID: uuid}
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
	if len(metrics) == 0 {
		fmt.Println("  No metrics collected")
		return
	}
	data, _ := json.Marshal(metrics)
	// Send to socket
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		fmt.Printf("  Socket send failed: %v\n", err)
		return
	}
	conn.Write(append(data, '\n'))
	conn.Close()
	fmt.Printf("  -> Sent to socket: sm_active=%.6f util=%.0f%% power=%.1fW temp=%.0fC\n",
		metrics[0].SMActive, metrics[0].GPUUtilization, metrics[0].PowerW, metrics[0].TemperatureC)
}

func agentListener(ctx context.Context, cwClient *cloudwatch.Client) {
	os.Remove(socketPath)
	os.MkdirAll("/var/run/ecs", 0755)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Printf("Agent listen error: %v\n", err)
		return
	}
	defer ln.Close()
	os.Chmod(socketPath, 0666)
	fmt.Println("Agent: listening on socket")

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			s := bufio.NewScanner(conn)
			s.Buffer(make([]byte, 64*1024), 64*1024)
			for s.Scan() {
				var metrics []GPUMetric
				if err := json.Unmarshal(s.Bytes(), &metrics); err != nil {
					continue
				}
				publishToCloudWatch(ctx, cwClient, metrics)
			}
		}()
	}
}

func publishToCloudWatch(ctx context.Context, cwClient *cloudwatch.Client, metrics []GPUMetric) {
	now := time.Now()
	var data []cwtypes.MetricDatum
	for _, m := range metrics {
		dims := []cwtypes.Dimension{{Name: aws.String("GPUUUID"), Value: aws.String(m.GPUUUID)}}
		data = append(data,
			cwtypes.MetricDatum{MetricName: aws.String("GPUUtilization"), Value: aws.Float64(m.GPUUtilization), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUMemoryUtilization"), Value: aws.Float64(m.MemoryUtilization), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUPowerDraw"), Value: aws.Float64(m.PowerW), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUTemperature"), Value: aws.Float64(m.TemperatureC), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUSMActive"), Value: aws.Float64(m.SMActive), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUSMOccupancy"), Value: aws.Float64(m.SMOccupancy), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUDRAMActive"), Value: aws.Float64(m.DRAMActive), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
		)
	}
	_, err := cwClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String("ECS/GPUMetrics"), MetricData: data,
	})
	if err != nil {
		fmt.Printf("  CloudWatch error: %v\n", err)
	} else {
		fmt.Printf("  CloudWatch: published %d metrics\n", len(data))
	}
}

func safeFloat(fv dcgm.FieldValue_v1) float64 {
	if fv.Status != 0 {
		return 0
	}
	v := fv.Float64()
	if v > 140737488355327.0 || v < 0 {
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
