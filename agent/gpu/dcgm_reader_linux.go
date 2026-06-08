//go:build linux

package gpu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/cihub/seelog"
)

const (
	dcgmSocketPath = "/var/run/ecs/gpu-metrics.sock"
	cwNamespace    = "ECS/GPUMetrics"
	reconnectDelay = 10 * time.Second
)

// DCGMMetric mirrors the JSON written by dcgm-init.
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

// StartDCGMMetricsReader connects to dcgm-init's Unix socket, reads GPU metrics,
// and publishes them to CloudWatch. Reconnects on failure.
func StartDCGMMetricsReader(ctx context.Context) {
	go readAndPublish(ctx)
}

func readAndPublish(ctx context.Context) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		seelog.Warnf("GPU metrics reader: AWS config error: %v", err)
		return
	}
	cwClient := cloudwatch.NewFromConfig(cfg)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := net.DialTimeout("unix", dcgmSocketPath, 5*time.Second)
		if err != nil {
			seelog.Debugf("GPU metrics reader: waiting for dcgm-init socket: %v", err)
			time.Sleep(reconnectDelay)
			continue
		}
		seelog.Infof("GPU metrics reader: connected to dcgm-init socket")

		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			var metrics []DCGMMetric
			if err := json.Unmarshal(scanner.Bytes(), &metrics); err != nil {
				continue
			}
			publishMetrics(ctx, cwClient, metrics)
		}
		conn.Close()
		seelog.Warnf("GPU metrics reader: disconnected, reconnecting...")
		time.Sleep(reconnectDelay)
	}
}

func publishMetrics(ctx context.Context, cwClient *cloudwatch.Client, metrics []DCGMMetric) {
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
		Namespace: aws.String(cwNamespace), MetricData: data,
	})
	if err != nil {
		seelog.Warnf("GPU metrics reader: CloudWatch error: %v", err)
	} else {
		fmt.Printf("GPU metrics: published %d metrics (sm_active=%.6f)\n", len(data), metrics[0].SMActive)
	}
}
