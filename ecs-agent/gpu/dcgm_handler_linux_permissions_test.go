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

	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 1, "Agent should be able to read GPU metrics file with 0644 perms")
	assert.Equal(t, "GPU-read-test", metrics[0].GPUUUID)
}
