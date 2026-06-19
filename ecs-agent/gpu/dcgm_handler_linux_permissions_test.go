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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGPUMetricsFile_ReadableByAgent verifies that the GPU metrics file
// can be read by any process (simulating ecs-agent reading).
func TestGPUMetricsFile_ReadableByAgent(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-read-test", GPUUtilization: aws.Float64(50.0)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	os.Chmod(filePath, 0644)

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 1, "Agent should be able to read GPU metrics file with 0644 perms")
	assert.Equal(t, "GPU-read-test", metrics[0].GPUUUID)
}

// TestGPUMetricsDir_MkdirAllIdempotent verifies that dcgm-init does not fail
// if the output directory already exists (race condition where ecs-init creates
// the directory before dcgm-init starts).
func TestGPUMetricsDir_MkdirAllIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	metricsDir := filepath.Join(tmpDir, "ecs")

	err := os.MkdirAll(metricsDir, 0755)
	require.NoError(t, err)

	info, err := os.Stat(metricsDir)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	// Simulate dcgm-init calling MkdirAll on the same path (should not fail)
	err = os.MkdirAll(metricsDir, 0755)
	assert.NoError(t, err, "MkdirAll should not fail when directory already exists")

	info, err = os.Stat(metricsDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

// TestGPUMetricsDir_ConcurrentCreation simulates a race condition where both
// ecs-init and dcgm-init attempt to create the directory concurrently.
func TestGPUMetricsDir_ConcurrentCreation(t *testing.T) {
	tmpDir := t.TempDir()
	metricsDir := filepath.Join(tmpDir, "ecs")

	const goroutines = 10
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			errs <- os.MkdirAll(metricsDir, 0755)
		}()
	}

	for i := 0; i < goroutines; i++ {
		err := <-errs
		assert.NoError(t, err, "Concurrent MkdirAll should never fail")
	}

	info, err := os.Stat(metricsDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}
