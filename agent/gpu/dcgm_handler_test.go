//go:build unit && linux

package gpu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrFloat64(v float64) *float64 { return &v }
func ptrUint64(v uint64) *uint64    { return &v }

func TestDCGMHandler_GetGPUMetrics_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{
				GPUUUID:            "GPU-abc-123",
				GPUUtilization:     ptrFloat64(85.0),
				MemoryUtilization:  ptrFloat64(50.0),
				MemoryTotal:        ptrUint64(16106127360),
				MemoryUsed:         ptrUint64(8053063680),
				PowerDraw:          ptrFloat64(250.5),
				Temperature:        ptrFloat64(72.0),
				RestartAppXidCount: 0,
			},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 1)
	assert.Equal(t, "GPU-abc-123", metrics[0].GPUUUID)
	assert.Equal(t, 85.0, *metrics[0].GPUUtilization)
	assert.Equal(t, 50.0, *metrics[0].MemoryUtilization)
	assert.Equal(t, uint64(16106127360), *metrics[0].MemoryTotal)
	assert.Equal(t, uint64(8053063680), *metrics[0].MemoryUsed)
	assert.Equal(t, 250.5, *metrics[0].PowerDraw)
	assert.Equal(t, 72.0, *metrics[0].Temperature)
	assert.Equal(t, int64(0), metrics[0].RestartAppXidCount)
}

func TestDCGMHandler_GetGPUMetrics_MultipleGPUs(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-0", GPUUtilization: ptrFloat64(100.0), Temperature: ptrFloat64(55.0)},
			{GPUUUID: "GPU-1", GPUUtilization: ptrFloat64(75.0), Temperature: ptrFloat64(50.0)},
			{GPUUUID: "GPU-2", GPUUtilization: ptrFloat64(50.0), Temperature: ptrFloat64(45.0)},
			{GPUUUID: "GPU-3", GPUUtilization: ptrFloat64(25.0), Temperature: ptrFloat64(40.0)},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 4)
	assert.Equal(t, "GPU-0", metrics[0].GPUUUID)
	assert.Equal(t, "GPU-3", metrics[3].GPUUUID)
	assert.Equal(t, 100.0, *metrics[0].GPUUtilization)
	assert.Equal(t, 25.0, *metrics[3].GPUUtilization)
}

func TestDCGMHandler_GetGPUMetrics_FileNotFound(t *testing.T) {
	handler := NewDCGMHandler("/nonexistent/path/gpu-metrics.json")
	metrics := handler.GetGPUMetrics()
	assert.Nil(t, metrics)
}

func TestDCGMHandler_GetGPUMetrics_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	os.WriteFile(filePath, []byte("not valid json{{{"), 0644)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()
	assert.Nil(t, metrics)
}

func TestDCGMHandler_GetGPUMetrics_StaleData(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write data with a timestamp 5 minutes ago (exceeds 2min staleness threshold)
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-stale", GPUUtilization: ptrFloat64(50.0)},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()
	assert.Nil(t, metrics, "Stale metrics should return nil")
}

func TestDCGMHandler_GetGPUMetrics_SameTimestampReturnsCached(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	ts := time.Now().UTC().Format(time.RFC3339)
	data := GPUMetricsFileData{
		Timestamp: ts,
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-cached", GPUUtilization: ptrFloat64(42.0)},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)

	// First call should parse and cache
	metrics1 := handler.GetGPUMetrics()
	require.Len(t, metrics1, 1)

	// Second call with same timestamp should return cached
	metrics2 := handler.GetGPUMetrics()
	require.Len(t, metrics2, 1)
	assert.Equal(t, metrics1[0].GPUUUID, metrics2[0].GPUUUID)
}

func TestDCGMHandler_GetGPUMetrics_FractionalGPU_NilPowerAndTemp(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Fractional vGPUs (g6f instances) don't report power or temperature
	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetricJSON{
			{
				GPUUUID:            "GPU-fractional",
				GPUUtilization:     ptrFloat64(0.0),
				MemoryUtilization:  ptrFloat64(7.5),
				MemoryTotal:        ptrUint64(6442450944),
				MemoryUsed:         ptrUint64(0),
				PowerDraw:          nil,
				Temperature:        nil,
				RestartAppXidCount: 0,
			},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 1)
	assert.Equal(t, "GPU-fractional", metrics[0].GPUUUID)
	assert.Nil(t, metrics[0].PowerDraw, "Fractional vGPU should not report power")
	assert.Nil(t, metrics[0].Temperature, "Fractional vGPU should not report temperature")
	assert.Equal(t, uint64(6442450944), *metrics[0].MemoryTotal)
}

func TestDCGMHandler_GetGPUMetrics_InvalidTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: "not-a-timestamp",
		GPUs: []GPUMetricJSON{
			{GPUUUID: "GPU-bad-ts"},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()
	assert.Nil(t, metrics, "Invalid timestamp should return nil")
}

func TestDCGMHandler_DefaultFilePath(t *testing.T) {
	handler := NewDCGMHandler("")
	assert.Equal(t, DefaultGPUMetricsFilePath, handler.filePath)
}

func writeMetricsFile(t *testing.T, path string, data GPUMetricsFileData) {
	t.Helper()
	bytes, err := json.MarshalIndent(data, "", "  ")
	require.NoError(t, err)
	err = os.WriteFile(path, bytes, 0644)
	require.NoError(t, err)
}
