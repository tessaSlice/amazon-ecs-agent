//go:build unit && linux

package gpu

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGPUMetrics_PopulatesTACSPayload_WhenAvailable verifies that the GPU metrics
// collector correctly populates the TACS payload (GeneralMetricsWrapper) when a
// mock dcgm-init reports valid metrics via the shared file.
func TestGPUMetrics_PopulatesTACSPayload_WhenAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Simulate dcgm-init writing valid metrics
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{
				GPUUUID:            "GPU-aaaa-1111",
				GPUUtilization:     ptrFloat64(95.0),
				MemoryUtilization:  ptrFloat64(60.0),
				MemoryTotal:        ptrUint64(16106127360),
				MemoryUsed:         ptrUint64(9663676416),
				PowerDraw:          ptrFloat64(280.0),
				Temperature:        ptrFloat64(78.0),
				RestartAppXidCount: 1,
			},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	// Simulate the agent reading and building a TACS payload
	handler := NewDCGMHandler(filePath)
	gpuMetrics := handler.GetGPUMetrics()
	require.NotNil(t, gpuMetrics, "GPU metrics should be available")

	// Build container-level payload (as the stats engine would)
	containerPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-aaaa-1111"})
	require.NotNil(t, containerPayload)

	// Simulate attaching to ContainerMetric
	containerMetric := &ecstcs.ContainerMetric{
		ContainerName:         aws.String("gpu-workload"),
		GeneralMetricsPayload: containerPayload,
	}

	// Verify the TACS payload structure is correct
	require.NotNil(t, containerMetric.GeneralMetricsPayload)
	require.Len(t, containerMetric.GeneralMetricsPayload, 1)

	wrapper := containerMetric.GeneralMetricsPayload[0]
	assert.Equal(t, "AcceleratedDevice", *wrapper.Dimensions[0].Key)
	assert.Equal(t, "GPU-aaaa-1111", *wrapper.Dimensions[0].Value)
	assert.Len(t, wrapper.GeneralMetrics, 7)

	// Build instance-level payload
	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, 1)
	instanceMetrics := &ecstcs.InstanceMetrics{
		GeneralMetricsPayload: instancePayload,
	}

	// Simulate building TelemetryMessage (what gets sent over the websocket)
	telemetryMessage := ecstcs.TelemetryMessage{
		InstanceMetrics: instanceMetrics,
		TaskMetrics: []*ecstcs.TaskMetric{
			{
				TaskArn:          aws.String("arn:aws:ecs:us-west-2:123456789:task/cluster/task-id"),
				ContainerMetrics: []*ecstcs.ContainerMetric{containerMetric},
			},
		},
	}

	// Verify the full TelemetryMessage
	require.NotNil(t, telemetryMessage.InstanceMetrics)
	require.Len(t, telemetryMessage.InstanceMetrics.GeneralMetricsPayload, 1)
	require.Len(t, telemetryMessage.TaskMetrics, 1)
	require.Len(t, telemetryMessage.TaskMetrics[0].ContainerMetrics, 1)
	require.NotNil(t, telemetryMessage.TaskMetrics[0].ContainerMetrics[0].GeneralMetricsPayload)

	// Verify JSON serialization works (what actually goes on the websocket)
	jsonBytes, err := json.Marshal(telemetryMessage)
	require.NoError(t, err)
	assert.Contains(t, string(jsonBytes), "GPUUtilization")
	assert.Contains(t, string(jsonBytes), "GPUTemperature")
	assert.Contains(t, string(jsonBytes), "InstanceGPULimit")
	assert.Contains(t, string(jsonBytes), "GPU-aaaa-1111")
}

// TestGPUMetrics_OmittedFromPayload_WhenNoGPUs verifies that the handler correctly
// omits GPU metrics when there are no GPUs available (file doesn't exist).
func TestGPUMetrics_OmittedFromPayload_WhenNoGPUs(t *testing.T) {
	// Point to a nonexistent file (simulates non-GPU instance or dcgm-init not running)
	handler := NewDCGMHandler("/nonexistent/gpu-metrics.json")
	gpuMetrics := handler.GetGPUMetrics()

	assert.Nil(t, gpuMetrics, "Should return nil when no GPU metrics file exists")

	// Container payload should be nil
	containerPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-any"})
	assert.Nil(t, containerPayload, "Container payload should be nil when no GPU metrics")

	// Instance payload should be nil
	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, 0)
	assert.Nil(t, instancePayload, "Instance payload should be nil when no GPU metrics")

	// TelemetryMessage should have nil InstanceMetrics (no GPU data to report)
	telemetryMessage := ecstcs.TelemetryMessage{
		InstanceMetrics: nil,
		TaskMetrics: []*ecstcs.TaskMetric{
			{
				TaskArn: aws.String("arn:aws:ecs:us-west-2:123456789:task/cluster/task-id"),
				ContainerMetrics: []*ecstcs.ContainerMetric{
					{
						ContainerName:         aws.String("non-gpu-container"),
						GeneralMetricsPayload: nil, // No GPU metrics attached
					},
				},
			},
		},
	}

	assert.Nil(t, telemetryMessage.InstanceMetrics)
	assert.Nil(t, telemetryMessage.TaskMetrics[0].ContainerMetrics[0].GeneralMetricsPayload)
}

// TestGPUMetrics_MultiContainerMultiGPU verifies the container-to-GPU mapping
// works correctly when multiple containers share a multi-GPU instance.
func TestGPUMetrics_MultiContainerMultiGPU(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// 4-GPU instance (like g4dn.12xlarge)
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-0", GPUUtilization: ptrFloat64(100.0), PowerDraw: ptrFloat64(70.0), Temperature: ptrFloat64(60.0)},
			{GPUUUID: "GPU-1", GPUUtilization: ptrFloat64(80.0), PowerDraw: ptrFloat64(65.0), Temperature: ptrFloat64(55.0)},
			{GPUUUID: "GPU-2", GPUUtilization: ptrFloat64(50.0), PowerDraw: ptrFloat64(50.0), Temperature: ptrFloat64(50.0)},
			{GPUUUID: "GPU-3", GPUUtilization: ptrFloat64(0.0), PowerDraw: ptrFloat64(9.0), Temperature: ptrFloat64(35.0)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	gpuMetrics := handler.GetGPUMetrics()
	require.Len(t, gpuMetrics, 4)

	// Container A has GPUs 0 and 1
	containerAPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-0", "GPU-1"})
	require.Len(t, containerAPayload, 2)

	// Container B has GPU 2
	containerBPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-2"})
	require.Len(t, containerBPayload, 1)
	assert.Equal(t, "GPU-2", *containerBPayload[0].Dimensions[0].Value)

	// Container C has no GPUs (CPU-only task)
	containerCPayload := GPUMetricsForContainer(gpuMetrics, nil)
	assert.Nil(t, containerCPayload)

	// GPU-3 is unassigned — should not appear in any container payload
	allAssigned := append(containerAPayload, containerBPayload...)
	for _, wrapper := range allAssigned {
		assert.NotEqual(t, "GPU-3", *wrapper.Dimensions[0].Value,
			"Unassigned GPU-3 should not appear in any container's payload")
	}

	// Instance-level should still report all 4 GPUs as the limit
	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, 3) // 3 GPUs assigned across containers
	require.NotNil(t, instancePayload)

	var limit, usage int64
	for _, gm := range instancePayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "InstanceGPULimit":
			limit = *gm.MetricValueLong
		case "InstanceGPUUsageTotal":
			usage = *gm.MetricValueLong
		}
	}
	assert.Equal(t, int64(4), limit, "All 4 GPUs on the instance")
	assert.Equal(t, int64(3), usage, "3 GPUs assigned to task containers")
}

// TestGPUMetrics_StaleData_NotIncludedInPayload verifies that stale GPU metrics
// (dcgm-init crashed/stopped) are NOT forwarded to TACS.
func TestGPUMetrics_StaleData_NotIncludedInPayload(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write data with old timestamp (simulates dcgm-init crashed 5 min ago)
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-stale", GPUUtilization: ptrFloat64(99.0)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	gpuMetrics := handler.GetGPUMetrics()

	assert.Nil(t, gpuMetrics, "Stale metrics should not be returned")

	// If metrics are nil, they should not appear in the TACS payload
	containerPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-stale"})
	assert.Nil(t, containerPayload, "Stale data should not be included in TACS payload")
}
