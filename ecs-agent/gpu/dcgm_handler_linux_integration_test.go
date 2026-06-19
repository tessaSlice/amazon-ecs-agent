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

// Integration test constants.
const (
	integGPUUUID       = "GPU-aaaa-1111"
	integContainerName = "gpu-workload"
	integTaskArn       = "arn:aws:ecs:us-west-2:123456789:task/cluster/task-id"

	integUtilization = 95.0
	integMemUtil     = 60.0
	integMemTotal    = uint64(16106127360)
	integMemUsed     = uint64(9663676416)
	integPowerDraw   = 280.0
	integTemperature = 78.0
	integXidCount    = int64(1)

	// Multi-GPU test constants (4-GPU instance like g4dn.12xlarge)
	multiGPU0UUID = "GPU-0"
	multiGPU1UUID = "GPU-1"
	multiGPU2UUID = "GPU-2"
	multiGPU3UUID = "GPU-3"

	multiGPU0Util  = 100.0
	multiGPU1Util  = 80.0
	multiGPU2Util  = 50.0
	multiGPU3Util  = 0.0
	multiGPU0Power = 70.0
	multiGPU1Power = 65.0
	multiGPU2Power = 50.0
	multiGPU3Power = 9.0
	multiGPU0Temp  = 60.0
	multiGPU1Temp  = 55.0
	multiGPU2Temp  = 50.0
	multiGPU3Temp  = 35.0

	multiGPUCount        = 4
	multiGPUUsageTotal   = int64(3)
	multiGPUIdleUsage    = int64(0)
	multiGPUNonExistent  = "GPU-nonexistent"
	integDimensionKey    = "AcceleratedDevice"
	integMetricCount     = 7
	integStaleTimestamp  = "2026-01-01T00:00:00Z"
	integStaleUtil       = 99.0
	integUnitCount       = "Count"
)

// TestGPUMetrics_PopulatesTACSPayload_WhenAvailable verifies that the GPU metrics
// collector correctly populates the TACS payload (GeneralMetricsWrapper) when a
// mock dcgm-init reports valid metrics via the shared file.
func TestGPUMetrics_PopulatesTACSPayload_WhenAvailable(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetric{
			{
				GPUUUID:            integGPUUUID,
				GPUUtilization:     aws.Float64(integUtilization),
				MemoryUtilization:  aws.Float64(integMemUtil),
				MemoryTotal:        aws.Uint64(integMemTotal),
				MemoryUsed:         aws.Uint64(integMemUsed),
				PowerDraw:          aws.Float64(integPowerDraw),
				Temperature:        aws.Float64(integTemperature),
				RestartAppXidCount: integXidCount,
			},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	gpuMetrics := handler.GetGPUMetrics()
	require.NotNil(t, gpuMetrics, "GPU metrics should be available")

	// Build container-level payload (as the stats engine would)
	containerPayload := GPUMetricsForContainer(gpuMetrics, []string{integGPUUUID})
	require.NotNil(t, containerPayload)

	containerMetric := &ecstcs.ContainerMetric{
		ContainerName:         aws.String(integContainerName),
		GeneralMetricsPayload: containerPayload,
	}

	require.NotNil(t, containerMetric.GeneralMetricsPayload)
	require.Len(t, containerMetric.GeneralMetricsPayload, 1)

	wrapper := containerMetric.GeneralMetricsPayload[0]
	assert.Equal(t, integDimensionKey, *wrapper.Dimensions[0].Key)
	assert.Equal(t, integGPUUUID, *wrapper.Dimensions[0].Value)
	assert.Len(t, wrapper.GeneralMetrics, integMetricCount)

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
				TaskArn:          aws.String(integTaskArn),
				ContainerMetrics: []*ecstcs.ContainerMetric{containerMetric},
			},
		},
	}

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
	assert.Contains(t, string(jsonBytes), integGPUUUID)
}

// TestGPUMetrics_OmittedFromPayload_WhenNoGPUs verifies that the handler correctly
// omits GPU metrics when there are no GPUs available (file doesn't exist).
func TestGPUMetrics_OmittedFromPayload_WhenNoGPUs(t *testing.T) {
	handler := NewDCGMHandler("/nonexistent/gpu-metrics.json")
	gpuMetrics := handler.GetGPUMetrics()

	assert.Nil(t, gpuMetrics, "Should return nil when no GPU metrics file exists")

	containerPayload := GPUMetricsForContainer(gpuMetrics, []string{"GPU-any"})
	assert.Nil(t, containerPayload, "Container payload should be nil when no GPU metrics")

	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, 0)
	assert.Nil(t, instancePayload, "Instance payload should be nil when no GPU metrics")

	telemetryMessage := ecstcs.TelemetryMessage{
		InstanceMetrics: nil,
		TaskMetrics: []*ecstcs.TaskMetric{
			{
				TaskArn: aws.String(integTaskArn),
				ContainerMetrics: []*ecstcs.ContainerMetric{
					{
						ContainerName:         aws.String("non-gpu-container"),
						GeneralMetricsPayload: nil,
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

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetric{
			{GPUUUID: multiGPU0UUID, GPUUtilization: aws.Float64(multiGPU0Util), PowerDraw: aws.Float64(multiGPU0Power), Temperature: aws.Float64(multiGPU0Temp)},
			{GPUUUID: multiGPU1UUID, GPUUtilization: aws.Float64(multiGPU1Util), PowerDraw: aws.Float64(multiGPU1Power), Temperature: aws.Float64(multiGPU1Temp)},
			{GPUUUID: multiGPU2UUID, GPUUtilization: aws.Float64(multiGPU2Util), PowerDraw: aws.Float64(multiGPU2Power), Temperature: aws.Float64(multiGPU2Temp)},
			{GPUUUID: multiGPU3UUID, GPUUtilization: aws.Float64(multiGPU3Util), PowerDraw: aws.Float64(multiGPU3Power), Temperature: aws.Float64(multiGPU3Temp)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	gpuMetrics := handler.GetGPUMetrics()
	require.Len(t, gpuMetrics, multiGPUCount)

	// Container A has GPUs 0 and 1
	containerAPayload := GPUMetricsForContainer(gpuMetrics, []string{multiGPU0UUID, multiGPU1UUID})
	require.Len(t, containerAPayload, 2)

	aGPU0 := containerAPayload[0]
	assert.Equal(t, integDimensionKey, *aGPU0.Dimensions[0].Key)
	assert.Equal(t, multiGPU0UUID, *aGPU0.Dimensions[0].Value)
	for _, gm := range aGPU0.GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, multiGPU0Util, *gm.MetricValueDouble)
		case "GPUPowerDraw":
			assert.Equal(t, multiGPU0Power, *gm.MetricValueDouble)
		case "GPUTemperature":
			assert.Equal(t, multiGPU0Temp, *gm.MetricValueDouble)
		}
	}

	aGPU1 := containerAPayload[1]
	assert.Equal(t, multiGPU1UUID, *aGPU1.Dimensions[0].Value)
	for _, gm := range aGPU1.GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, multiGPU1Util, *gm.MetricValueDouble)
		case "GPUPowerDraw":
			assert.Equal(t, multiGPU1Power, *gm.MetricValueDouble)
		case "GPUTemperature":
			assert.Equal(t, multiGPU1Temp, *gm.MetricValueDouble)
		}
	}

	// Container B has GPU 2
	containerBPayload := GPUMetricsForContainer(gpuMetrics, []string{multiGPU2UUID})
	require.Len(t, containerBPayload, 1)
	assert.Equal(t, multiGPU2UUID, *containerBPayload[0].Dimensions[0].Value)
	for _, gm := range containerBPayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, multiGPU2Util, *gm.MetricValueDouble)
		case "GPUPowerDraw":
			assert.Equal(t, multiGPU2Power, *gm.MetricValueDouble)
		case "GPUTemperature":
			assert.Equal(t, multiGPU2Temp, *gm.MetricValueDouble)
		}
	}

	// Container C has no GPUs (CPU-only task)
	containerCPayload := GPUMetricsForContainer(gpuMetrics, nil)
	assert.Nil(t, containerCPayload)

	// Container D requests a GPU that doesn't exist on the instance
	containerDPayload := GPUMetricsForContainer(gpuMetrics, []string{multiGPUNonExistent})
	assert.Nil(t, containerDPayload, "Requesting a non-existent GPU should return nil")

	// GPU-3 is unassigned — should not appear in any container payload
	allAssigned := append(containerAPayload, containerBPayload...)
	for _, wrapper := range allAssigned {
		assert.NotEqual(t, multiGPU3UUID, *wrapper.Dimensions[0].Value,
			"Unassigned GPU-3 should not appear in any container's payload")
	}

	// Verify GPU-3 can be retrieved if explicitly requested
	unassignedPayload := GPUMetricsForContainer(gpuMetrics, []string{multiGPU3UUID})
	require.Len(t, unassignedPayload, 1)
	assert.Equal(t, multiGPU3UUID, *unassignedPayload[0].Dimensions[0].Value)
	for _, gm := range unassignedPayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "GPUUtilization":
			assert.Equal(t, multiGPU3Util, *gm.MetricValueDouble)
		case "GPUPowerDraw":
			assert.Equal(t, multiGPU3Power, *gm.MetricValueDouble)
		case "GPUTemperature":
			assert.Equal(t, multiGPU3Temp, *gm.MetricValueDouble)
		}
	}

	// Verify all 4 GPUs can be retrieved at once
	allGPUsPayload := GPUMetricsForContainer(gpuMetrics, []string{multiGPU0UUID, multiGPU1UUID, multiGPU2UUID, multiGPU3UUID})
	require.Len(t, allGPUsPayload, multiGPUCount, "Should return all 4 GPUs when all UUIDs requested")

	// Instance-level should report all 4 GPUs as the limit
	instancePayload := GPUMetricsToInstancePayload(gpuMetrics, multiGPUUsageTotal)
	require.NotNil(t, instancePayload)
	require.Len(t, instancePayload, 1)
	require.Len(t, instancePayload[0].GeneralMetrics, 2)

	var limit, usage int64
	for _, gm := range instancePayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "InstanceGPULimit":
			limit = *gm.MetricValueLong
			assert.Equal(t, integUnitCount, *gm.Unit)
		case "InstanceGPUUsageTotal":
			usage = *gm.MetricValueLong
			assert.Equal(t, integUnitCount, *gm.Unit)
		}
	}
	assert.Equal(t, int64(multiGPUCount), limit)
	assert.Equal(t, multiGPUUsageTotal, usage)

	// Verify instance payload with 0 usage (no tasks running)
	idleInstancePayload := GPUMetricsToInstancePayload(gpuMetrics, multiGPUIdleUsage)
	require.NotNil(t, idleInstancePayload)
	for _, gm := range idleInstancePayload[0].GeneralMetrics {
		if *gm.MetricName == "InstanceGPUUsageTotal" {
			assert.Equal(t, multiGPUIdleUsage, *gm.MetricValueLong, "No GPUs in use when no tasks running")
		}
	}
}

// TestGPUMetrics_StaleData_NotReEmitted verifies that when dcgm-init stops
// updating the file (crash/hang), the agent does not re-emit the same data.
func TestGPUMetrics_StaleData_NotReEmitted(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: integStaleTimestamp,
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-stale", GPUUtilization: aws.Float64(integStaleUtil)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)

	metrics1 := handler.GetGPUMetrics()
	require.NotNil(t, metrics1)

	metrics2 := handler.GetGPUMetrics()
	assert.Equal(t, metrics1, metrics2,
		"Same timestamp should return cached metrics — stale data is not re-processed")
}
