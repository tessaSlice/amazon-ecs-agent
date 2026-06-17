//go:build unit && linux

package gpu

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPUMetricToGeneralMetricsWrapper_AllFields(t *testing.T) {
	metric := GPUMetric{
		GPUUUID:            "GPU-abc-123",
		GPUUtilization:     ptrFloat64(85.0),
		MemoryUtilization:  ptrFloat64(50.0),
		MemoryTotal:        ptrUint64(16106127360),
		MemoryUsed:         ptrUint64(8053063680),
		PowerDraw:          ptrFloat64(250.5),
		Temperature:        ptrFloat64(72.0),
		RestartAppXidCount: 3,
	}

	wrapper := GPUMetricToGeneralMetricsWrapper(metric)

	require.NotNil(t, wrapper)
	require.Len(t, wrapper.Dimensions, 1)
	assert.Equal(t, "AcceleratedDevice", *wrapper.Dimensions[0].Key)
	assert.Equal(t, "GPU-abc-123", *wrapper.Dimensions[0].Value)

	// Should have 6 metrics: utilization, mem_util, mem_total, power, temp, xid_count
	require.Len(t, wrapper.GeneralMetrics, 6)

	metricMap := make(map[string]interface{})
	for _, gm := range wrapper.GeneralMetrics {
		if gm.MetricValueDouble != nil {
			metricMap[*gm.MetricName] = *gm.MetricValueDouble
		} else if gm.MetricValueLong != nil {
			metricMap[*gm.MetricName] = *gm.MetricValueLong
		}
	}

	assert.Equal(t, 85.0, metricMap["GPUUtilization"])
	assert.Equal(t, 50.0, metricMap["GPUMemoryUtilization"])
	assert.Equal(t, int64(16106127360), metricMap["GPUMemoryTotal"])
	assert.Equal(t, 250.5, metricMap["GPUPowerDraw"])
	assert.Equal(t, 72.0, metricMap["GPUTemperature"])
	assert.Equal(t, int64(3), metricMap["GPURestartAppXidCount"])
}

func TestGPUMetricToGeneralMetricsWrapper_NilFields_FractionalGPU(t *testing.T) {
	// Fractional vGPUs (g6f) don't report power or temperature
	metric := GPUMetric{
		GPUUUID:            "GPU-fractional",
		GPUUtilization:     ptrFloat64(0.0),
		MemoryUtilization:  ptrFloat64(7.5),
		MemoryTotal:        ptrUint64(6442450944),
		MemoryUsed:         ptrUint64(0),
		PowerDraw:          nil,
		Temperature:        nil,
		RestartAppXidCount: 0,
	}

	wrapper := GPUMetricToGeneralMetricsWrapper(metric)

	require.NotNil(t, wrapper)
	// Should have 4 metrics: utilization, mem_util, mem_total, xid_count (no power, no temp)
	require.Len(t, wrapper.GeneralMetrics, 4)

	metricNames := make(map[string]bool)
	for _, gm := range wrapper.GeneralMetrics {
		metricNames[*gm.MetricName] = true
	}

	assert.True(t, metricNames["GPUUtilization"])
	assert.True(t, metricNames["GPUMemoryUtilization"])
	assert.True(t, metricNames["GPUMemoryTotal"])
	assert.True(t, metricNames["GPURestartAppXidCount"])
	assert.False(t, metricNames["GPUPowerDraw"], "Power should not be emitted for fractional vGPU")
	assert.False(t, metricNames["GPUTemperature"], "Temperature should not be emitted for fractional vGPU")
}

func TestGPUMetricToGeneralMetricsWrapper_AllNilFields(t *testing.T) {
	metric := GPUMetric{
		GPUUUID: "GPU-empty",
	}

	wrapper := GPUMetricToGeneralMetricsWrapper(metric)
	assert.Nil(t, wrapper, "All-nil metrics should return nil wrapper")
}

func TestGPUMetricToGeneralMetricsWrapper_Units(t *testing.T) {
	metric := GPUMetric{
		GPUUUID:            "GPU-units-test",
		GPUUtilization:     ptrFloat64(50.0),
		MemoryUtilization:  ptrFloat64(25.0),
		MemoryTotal:        ptrUint64(1024),
		MemoryUsed:         ptrUint64(512),
		PowerDraw:          ptrFloat64(100.0),
		Temperature:        ptrFloat64(60.0),
		RestartAppXidCount: 1,
	}

	wrapper := GPUMetricToGeneralMetricsWrapper(metric)
	require.NotNil(t, wrapper)

	unitMap := make(map[string]string)
	for _, gm := range wrapper.GeneralMetrics {
		unitMap[*gm.MetricName] = *gm.Unit
	}

	assert.Equal(t, "Percent", unitMap["GPUUtilization"])
	assert.Equal(t, "Percent", unitMap["GPUMemoryUtilization"])
	assert.Equal(t, "Bytes", unitMap["GPUMemoryTotal"])
	assert.Equal(t, "None", unitMap["GPUPowerDraw"])
	assert.Equal(t, "None", unitMap["GPUTemperature"])
	assert.Equal(t, "Count", unitMap["GPURestartAppXidCount"])
}

func TestGPUMetricsToInstancePayload(t *testing.T) {
	metrics := []GPUMetric{
		{GPUUUID: "GPU-0"},
		{GPUUUID: "GPU-1"},
		{GPUUUID: "GPU-2"},
		{GPUUUID: "GPU-3"},
	}

	payload := GPUMetricsToInstancePayload(metrics, 2)

	require.NotNil(t, payload)
	require.Len(t, payload, 1)
	require.Len(t, payload[0].GeneralMetrics, 2)

	metricMap := make(map[string]int64)
	for _, gm := range payload[0].GeneralMetrics {
		metricMap[*gm.MetricName] = *gm.MetricValueLong
	}

	assert.Equal(t, int64(4), metricMap["InstanceGPULimit"], "Should report total GPU count")
	assert.Equal(t, int64(2), metricMap["InstanceGPUUsageTotal"], "Should report GPUs in use")
}

func TestGPUMetricsToInstancePayload_Empty(t *testing.T) {
	payload := GPUMetricsToInstancePayload(nil, 0)
	assert.Nil(t, payload)

	payload = GPUMetricsToInstancePayload([]GPUMetric{}, 0)
	assert.Nil(t, payload)
}

func TestGPUMetricsForContainer_MatchesByUUID(t *testing.T) {
	metrics := []GPUMetric{
		{GPUUUID: "GPU-0", GPUUtilization: ptrFloat64(100.0), Temperature: ptrFloat64(70.0)},
		{GPUUUID: "GPU-1", GPUUtilization: ptrFloat64(50.0), Temperature: ptrFloat64(60.0)},
		{GPUUUID: "GPU-2", GPUUtilization: ptrFloat64(25.0), Temperature: ptrFloat64(50.0)},
		{GPUUUID: "GPU-3", GPUUtilization: ptrFloat64(0.0), Temperature: ptrFloat64(40.0)},
	}

	// Container has GPUs 1 and 3 assigned
	result := GPUMetricsForContainer(metrics, []string{"GPU-1", "GPU-3"})

	require.Len(t, result, 2)
	// Verify we got the right GPUs by checking dimensions
	uuids := make([]string, len(result))
	for i, wrapper := range result {
		uuids[i] = *wrapper.Dimensions[0].Value
	}
	assert.Contains(t, uuids, "GPU-1")
	assert.Contains(t, uuids, "GPU-3")
}

func TestGPUMetricsForContainer_NoMatchingGPUs(t *testing.T) {
	metrics := []GPUMetric{
		{GPUUUID: "GPU-0", GPUUtilization: ptrFloat64(100.0)},
		{GPUUUID: "GPU-1", GPUUtilization: ptrFloat64(50.0)},
	}

	result := GPUMetricsForContainer(metrics, []string{"GPU-99"})
	assert.Nil(t, result)
}

func TestGPUMetricsForContainer_EmptyInputs(t *testing.T) {
	assert.Nil(t, GPUMetricsForContainer(nil, []string{"GPU-0"}))
	assert.Nil(t, GPUMetricsForContainer([]GPUMetric{}, []string{"GPU-0"}))
	assert.Nil(t, GPUMetricsForContainer([]GPUMetric{{GPUUUID: "GPU-0"}}, nil))
	assert.Nil(t, GPUMetricsForContainer([]GPUMetric{{GPUUUID: "GPU-0"}}, []string{}))
}

func TestGPUMetricsForContainer_SingleGPU(t *testing.T) {
	metrics := []GPUMetric{
		{
			GPUUUID:            "GPU-single",
			GPUUtilization:     ptrFloat64(99.0),
			MemoryUtilization:  ptrFloat64(80.0),
			MemoryTotal:        ptrUint64(16106127360),
			MemoryUsed:         ptrUint64(12884901888),
			PowerDraw:          ptrFloat64(300.0),
			Temperature:        ptrFloat64(85.0),
			RestartAppXidCount: 2,
		},
	}

	result := GPUMetricsForContainer(metrics, []string{"GPU-single"})

	require.Len(t, result, 1)
	assert.Equal(t, "GPU-single", *result[0].Dimensions[0].Value)
	// 6 metrics: util, mem_util, mem_total, power, temp, xid_count
	assert.Len(t, result[0].GeneralMetrics, 6)
}

// TestGPUMetricNames_MatchTACSAllowlist verifies that the metric names emitted
// by dcgm-init match the TACS ALLOWED_GENERAL_METRIC_NAMES allowlist.
// Reference: MadisonTelemetryAgentCommunicationService
func TestGPUMetricNames_MatchTACSAllowlist(t *testing.T) {
	// These are the exact names from the TACS allowlist
	tacsAllowedNames := map[string]bool{
		"GPUUtilization":           true,
		"GPUMemoryUtilization":     true,
		"GPUMemoryTotal":           true,
		"GPUPowerDraw":             true,
		"GPUTemperature":           true,
		"GPUTensorCoreUtilization": true,
		"GPURestartAppXidCount":    true,
		"SPUSMActive":              true,
		"InstanceGPULimit":         true,
		"InstanceGPUUsageTotal":    true,
	}

	// Verify our emitted constants are all in the allowlist
	assert.True(t, tacsAllowedNames[gpuMetricNameGPUUtilization])
	assert.True(t, tacsAllowedNames[gpuMetricNameGPUMemoryUtilization])
	assert.True(t, tacsAllowedNames[gpuMetricNameGPUMemoryTotal])
	assert.True(t, tacsAllowedNames[gpuMetricNameGPUPowerDraw])
	assert.True(t, tacsAllowedNames[gpuMetricNameGPUTemperature])
	assert.True(t, tacsAllowedNames[gpuMetricNameGPURestartAppXidCount])
	assert.True(t, tacsAllowedNames[gpuMetricNameInstanceGPULimitCount])
	assert.True(t, tacsAllowedNames[gpuMetricNameInstanceGPUUsageTotal])
}

// TestGPUMetrics_EndToEnd_PublishFlow simulates the full flow:
// dcgm-init writes file -> handler reads -> conversion -> TACS payload ready
func TestGPUMetrics_EndToEnd_PublishFlow(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Simulate dcgm-init writing metrics (as it would on a g4dn.12xlarge under load)
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{
				GPUUUID:            "GPU-aaaa-1111",
				GPUUtilization:     ptrFloat64(100.0),
				MemoryUtilization:  ptrFloat64(45.0),
				MemoryTotal:        ptrUint64(16106127360),
				MemoryUsed:         ptrUint64(7247757312),
				PowerDraw:          ptrFloat64(70.5),
				Temperature:        ptrFloat64(58.0),
				RestartAppXidCount: 0,
			},
			{
				GPUUUID:            "GPU-bbbb-2222",
				GPUUtilization:     ptrFloat64(95.0),
				MemoryUtilization:  ptrFloat64(30.0),
				MemoryTotal:        ptrUint64(16106127360),
				MemoryUsed:         ptrUint64(4831838208),
				PowerDraw:          ptrFloat64(68.0),
				Temperature:        ptrFloat64(55.0),
				RestartAppXidCount: 0,
			},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	// Step 1: Handler reads the file (simulates agent reading from bind mount)
	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()
	require.Len(t, metrics, 2, "Handler should read 2 GPUs from the shared file")

	// Step 2: Convert to container-level payload (container has GPU-aaaa-1111 assigned)
	containerPayload := GPUMetricsForContainer(metrics, []string{"GPU-aaaa-1111"})
	require.Len(t, containerPayload, 1, "Container should have 1 GPU wrapper")
	assert.Equal(t, "GPU-aaaa-1111", *containerPayload[0].Dimensions[0].Value)
	assert.Len(t, containerPayload[0].GeneralMetrics, 6, "Should have all 6 metrics for full GPU")

	// Step 3: Convert to instance-level payload
	instancePayload := GPUMetricsToInstancePayload(metrics, 1) // 1 GPU assigned to tasks
	require.Len(t, instancePayload, 1)
	require.Len(t, instancePayload[0].GeneralMetrics, 2)

	var limit, usage int64
	for _, gm := range instancePayload[0].GeneralMetrics {
		switch *gm.MetricName {
		case "InstanceGPULimit":
			limit = *gm.MetricValueLong
		case "InstanceGPUUsageTotal":
			usage = *gm.MetricValueLong
		}
	}
	assert.Equal(t, int64(2), limit, "InstanceGPULimit should be 2 (total GPUs on instance)")
	assert.Equal(t, int64(1), usage, "InstanceGPUUsageTotal should be 1 (GPUs assigned to tasks)")
}
