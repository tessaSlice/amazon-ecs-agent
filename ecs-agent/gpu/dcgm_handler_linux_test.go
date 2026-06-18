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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDCGMHandler_GetGPUMetrics_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetric{
			{
				GPUUUID:            "GPU-abc-123",
				GPUUtilization:     aws.Float64(85.0),
				MemoryUtilization:  aws.Float64(50.0),
				MemoryTotal:        aws.Uint64(16106127360),
				MemoryUsed:         aws.Uint64(8053063680),
				PowerDraw:          aws.Float64(250.5),
				Temperature:        aws.Float64(72.0),
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
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-0", GPUUtilization: aws.Float64(100.0), Temperature: aws.Float64(55.0)},
			{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(75.0), Temperature: aws.Float64(50.0)},
			{GPUUUID: "GPU-2", GPUUtilization: aws.Float64(50.0), Temperature: aws.Float64(45.0)},
			{GPUUUID: "GPU-3", GPUUtilization: aws.Float64(25.0), Temperature: aws.Float64(40.0)},
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

func TestDCGMHandler_GetGPUMetrics_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	os.WriteFile(filePath, []byte(""), 0644)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()
	assert.Nil(t, metrics, "Empty file should return nil (JSON parsing fails gracefully)")
}

// TestDCGMHandler_GetGPUMetrics_StaleData_SameTimestamp verifies that when dcgm-init
// stops updating (crash/hang), the agent returns the cached metrics rather than
// re-emitting duplicates. The "stale" condition is detected by the timestamp not changing.
func TestDCGMHandler_GetGPUMetrics_StaleData_SameTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write data with a fixed old timestamp (simulating dcgm-init stopped updating)
	data := GPUMetricsFileData{
		Timestamp: "2026-01-01T00:00:00Z",
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-stale", GPUUtilization: aws.Float64(50.0)},
		},
		Healthy: true,
	}

	writeMetricsFile(t, filePath, data)

	handler := NewDCGMHandler(filePath)

	// First call reads and caches
	metrics1 := handler.GetGPUMetrics()
	require.Len(t, metrics1, 1)
	assert.Equal(t, "GPU-stale", metrics1[0].GPUUUID)

	// Second call with same file (timestamp unchanged) returns cached data
	metrics2 := handler.GetGPUMetrics()
	require.Len(t, metrics2, 1)
	assert.Equal(t, metrics1[0].GPUUUID, metrics2[0].GPUUUID,
		"Same timestamp should return cached metrics (stale data not re-emitted)")
}

func TestDCGMHandler_GetGPUMetrics_SameTimestampReturnsCached(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	ts := time.Now().UTC().Format(time.RFC3339)
	data := GPUMetricsFileData{
		Timestamp: ts,
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-cached", GPUUtilization: aws.Float64(42.0)},
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
		GPUs: []GPUMetric{
			{
				GPUUUID:            "GPU-fractional",
				GPUUtilization:     aws.Float64(0.0),
				MemoryUtilization:  aws.Float64(7.5),
				MemoryTotal:        aws.Uint64(6442450944),
				MemoryUsed:         aws.Uint64(0),
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

// TestDCGMHandler_GetGPUMetrics_InvalidTimestamp verifies that the handler rejects
// a file with an unparseable timestamp (corrupt file protection).
func TestDCGMHandler_GetGPUMetrics_InvalidTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: "not-a-timestamp",
		GPUs: []GPUMetric{
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
