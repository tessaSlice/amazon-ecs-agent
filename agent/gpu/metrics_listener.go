//go:build linux

package gpu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/cihub/seelog"
)

const (
	socketPath     = "/var/run/ecs/gpu-metrics.sock"
	cwNamespace    = "ECS/GPUMetrics"
)

// DCGMMetric mirrors the JSON written by ecs-init's DCGM collector.
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

// StartGPUMetricsListener starts a Unix socket listener that receives GPU metrics
// from ecs-init and publishes them to CloudWatch.
func StartGPUMetricsListener(ctx context.Context) {
	go listenAndPublish(ctx)
}

func listenAndPublish(ctx context.Context) {
	os.Remove(socketPath)
	os.MkdirAll("/var/run/ecs", 0755)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		seelog.Warnf("GPU metrics: failed to listen on %s: %v", socketPath, err)
		return
	}
	defer ln.Close()
	os.Chmod(socketPath, 0666)
	seelog.Infof("GPU metrics: listening on %s", socketPath)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		seelog.Warnf("GPU metrics: failed to load AWS config: %v", err)
		return
	}
	cwClient := cloudwatch.NewFromConfig(cfg)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				seelog.Warnf("GPU metrics: accept error: %v", err)
				continue
			}
		}

		go handleConnection(ctx, conn, cwClient)
	}
}

func handleConnection(ctx context.Context, conn net.Conn, cwClient *cloudwatch.Client) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		var metrics []DCGMMetric
		if err := json.Unmarshal(scanner.Bytes(), &metrics); err != nil {
			seelog.Warnf("GPU metrics: unmarshal error: %v", err)
			continue
		}
		publishToCloudWatch(ctx, cwClient, metrics)
	}
}

func publishToCloudWatch(ctx context.Context, cwClient *cloudwatch.Client, metrics []DCGMMetric) {
	var metricData []cwtypes.MetricDatum
	now := time.Now()

	for _, m := range metrics {
		dims := []cwtypes.Dimension{
			{Name: aws.String("GPUUUID"), Value: aws.String(m.GPUUUID)},
		}

		metricData = append(metricData,
			cwtypes.MetricDatum{MetricName: aws.String("GPUUtilization"), Value: aws.Float64(m.GPUUtilization), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUMemoryUtilization"), Value: aws.Float64(m.MemoryUtilization), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUMemoryUsed"), Value: aws.Float64(float64(m.MemoryUsedBytes)), Unit: cwtypes.StandardUnitBytes, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUMemoryTotal"), Value: aws.Float64(float64(m.MemoryTotalBytes)), Unit: cwtypes.StandardUnitBytes, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUPowerDraw"), Value: aws.Float64(m.PowerW), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUTemperature"), Value: aws.Float64(m.TemperatureC), Unit: cwtypes.StandardUnitNone, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUSMActive"), Value: aws.Float64(m.SMActive), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUSMOccupancy"), Value: aws.Float64(m.SMOccupancy), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
			cwtypes.MetricDatum{MetricName: aws.String("GPUDRAMActive"), Value: aws.Float64(m.DRAMActive), Unit: cwtypes.StandardUnitPercent, Dimensions: dims, Timestamp: &now},
		)
	}

	// CloudWatch allows max 1000 metrics per PutMetricData call.
	for i := 0; i < len(metricData); i += 20 {
		end := i + 20
		if end > len(metricData) {
			end = len(metricData)
		}
		_, err := cwClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(cwNamespace),
			MetricData: metricData[i:end],
		})
		if err != nil {
			seelog.Warnf("GPU metrics: CloudWatch put error: %v", err)
		} else {
			seelog.Debugf("GPU metrics: published %d metrics to CloudWatch", end-i)
		}
	}
	fmt.Printf("GPU metrics published: %d GPUs, sm_active=%.4f\n", len(metrics), metrics[0].SMActive)
}
