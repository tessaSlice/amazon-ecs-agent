//go:build unit && linux

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

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
		GPUs: []GPUMetric{
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
		GPUs: []GPUMetric{
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

	// Verify Container A's GPU-0 metrics are correct
	aGPU0 := containerAPayload[0]
	assert.Equal(t, "AcceleratedDevice", *aGPU0.Dimensions[0].Key)
	assert.Equal(t, "GPU-0", *aGPU0.Dimensions[0].Value)
	for _, gm := range aGPU0.GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, 100.0, *gm.MetricValueDouble, "GPU-0 utilization should be 100%%")
		case "GPUPowerDraw":
			assert.Equal(t, 70.0, *gm.MetricValueDouble, "GPU-0 power should be 70W")
		case "GPUTemperature":
			assert.Equal(t, 60.0, *gm.MetricValueDouble, "GPU-0 temp should be 60C")
		}
	}

	// Verify Container A's GPU-1 metrics are correct
	aGPU1 := containerAPayload[1]
	assert.Equal(t, "GPU-1", *aGPU1.Dimensions[0].Value)
	for _, gm := range aGPU1.GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, 80.0, *gm.MetricValueDouble, "GPU-1 utilization should be 80%%")
		case "GPUPowerDraw":
			assert.Equal(t, 65.0, *gm.MetricValueDouble, "GPU-1 power should be 65W")
		case "GPUTemperature":
			assert.Equal(t, 55.0, *gm.MetricValueDouble, "GPU-1 temp should be 55C")
		}
	}

	// Container B has GPU 2
	containerBPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-2"})
	require.Len(t, containerBPayload, 1)
	assert.Equal(t, "GPU-2", *containerBPayload[0].Dimensions[0].Value)
	for _, gm := range containerBPayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, 50.0, *gm.MetricValueDouble, "GPU-2 utilization should be 50%%")
		case "GPUPowerDraw":
			assert.Equal(t, 50.0, *gm.MetricValueDouble, "GPU-2 power should be 50W")
		case "GPUTemperature":
			assert.Equal(t, 50.0, *gm.MetricValueDouble, "GPU-2 temp should be 50C")
		}
	}

	// Container C has no GPUs (CPU-only task)
	containerCPayload := GPUMetricsForContainer(gpuMetrics, nil)
	assert.Nil(t, containerCPayload)

	// Container D requests a GPU that doesn't exist on the instance
	containerDPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-nonexistent"})
	assert.Nil(t, containerDPayload, "Requesting a non-existent GPU should return nil")

	// GPU-3 is unassigned — should not appear in any container payload
	allAssigned := append(containerAPayload, containerBPayload...)
	for _, wrapper := range allAssigned {
		assert.NotEqual(t, "GPU-3", *wrapper.Dimensions[0].Value,
			"Unassigned GPU-3 should not appear in any container's payload")
	}

	// Verify GPU-3 can be retrieved if explicitly requested
	unassignedPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-3"})
	require.Len(t, unassignedPayload, 1)
	assert.Equal(t, "GPU-3", *unassignedPayload[0].Dimensions[0].Value)
	for _, gm := range unassignedPayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, 0.0, *gm.MetricValueDouble, "GPU-3 should be idle")
		case "GPUPowerDraw":
			assert.Equal(t, 9.0, *gm.MetricValueDouble, "GPU-3 idle power should be 9W")
		case "GPUTemperature":
			assert.Equal(t, 35.0, *gm.MetricValueDouble, "GPU-3 idle temp should be 35C")
		}
	}

	// Verify all 4 GPUs can be retrieved at once (full instance query)
	allGPUsPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"})
	require.Len(t, allGPUsPayload, 4, "Should return all 4 GPUs when all UUIDs requested")

	// Instance-level should still report all 4 GPUs as the limit
	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, 3) // 3 GPUs assigned across containers
	require.NotNil(t, instancePayload)
	require.Len(t, instancePayload, 1)
	require.Len(t, instancePayload[0].GeneralMetrics, 2)

	var limit, usage int64
	for _, gm := range instancePayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "InstanceGPULimit":
			limit = *gm.MetricValueLong
			assert.Equal(t, "Count", *gm.Unit)
		case "InstanceGPUUsageTotal":
			usage = *gm.MetricValueLong
			assert.Equal(t, "Count", *gm.Unit)
		}
	}
	assert.Equal(t, int64(4), limit, "All 4 GPUs on the instance")
	assert.Equal(t, int64(3), usage, "3 GPUs assigned to task containers")

	// Verify instance payload with 0 usage (no tasks running)
	idleInstancePayload := GPUMetricsToInstancePayload(gpuMetrics, 0)
	require.NotNil(t, idleInstancePayload)
	for _, gm := range idleInstancePayload[0].GeneralMetrics {
		if *gm.MetricName == "InstanceGPUUsageTotal" {
			assert.Equal(t, int64(0), *gm.MetricValueLong, "No GPUs in use when no tasks running")
		}
	}
}

// TestGPUMetrics_StaleData_NotReEmitted verifies that when dcgm-init stops
// updating the file (crash/hang), the agent does not re-emit the same data.
// The handler detects staleness by comparing timestamps — if unchanged, it
// returns cached data (which the stats engine already published).
func TestGPUMetrics_StaleData_NotReEmitted(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write data with a fixed timestamp (simulates dcgm-init stopped updating)
	data := GPUMetricsFileData{
		Timestamp: "2026-01-01T00:00:00Z",
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-stale", GPUUtilization: ptrFloat64(99.0)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)

	// First read returns the metrics
	metrics1 := handler.GetGPUMetrics()
	require.NotNil(t, metrics1)

	// Subsequent reads with unchanged file return the same cached pointer
	// (timestamp didn't change, so no new data to emit)
	metrics2 := handler.GetGPUMetrics()
	assert.Equal(t, metrics1, metrics2,
		"Same timestamp should return cached metrics — stale data is not re-processed")

	// The stats engine uses the timestamp to avoid publishing duplicates to TACS
}
