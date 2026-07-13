//go:build unit && linux

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//      http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package doctor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/gpu"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinClockNear pins the package timeNow to a fixed instant just after the RFC3339
// timestamp the test writes into the metrics file, so the file reads as fresh
// (within gpuMetricsMaxStaleness) rather than being flagged stale by RunCheck.
// It restores the original clock on test cleanup.
func pinClockNear(t *testing.T, fileTimestamp string) {
	t.Helper()
	base, err := time.Parse(time.RFC3339, fileTimestamp)
	require.NoError(t, err)
	original := timeNow
	timeNow = func() time.Time { return base.Add(time.Second) }
	t.Cleanup(func() { timeNow = original })
}

func TestGPUHealthcheckReportsOkWhenHealthy(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	pinClockNear(t, "2026-01-01T00:00:00Z")
	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": true,
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, status)
	assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, hc.GetHealthcheckType())
}

func TestGPUHealthcheckReportsImpairedWhenUnhealthy(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	pinClockNear(t, "2026-01-01T00:00:00Z")
	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": false,
		"unhealthy_reason": "XID_48",
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusImpaired, status)
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileMissing(t *testing.T) {
	handler := gpu.NewDCGMHandler("/nonexistent/gpu-metrics.json")
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status)
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileCorrupt(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`not valid json{{{`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status)
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(""), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status)
}

// connection_lost=true means dcgm-init cannot determine GPU health, so the check
// must report INSUFFICIENT_DATA even though healthy=true (dcgm-init leaves Healthy
// true while disconnected).
func TestGPUHealthcheckReportsInsufficientDataWhenConnectionLost(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": true,
		"connection_lost": true,
		"gpus": []
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status)
}

// A healthy=true, connection_lost=false file whose timestamp is older than
// gpuMetricsMaxStaleness means dcgm-init is dead or hung (it can no longer set
// connection_lost), so the check must report INSUFFICIENT_DATA rather than a
// false OK.
func TestGPUHealthcheckReportsInsufficientDataWhenStale(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": true,
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	// Advance the clock well past the staleness window from the file timestamp.
	base, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	original := timeNow
	timeNow = func() time.Time { return base.Add(gpuMetricsMaxStaleness + time.Minute) }
	defer func() { timeNow = original }()

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status,
		"a stale metrics file (dcgm-init dead/hung) must report INSUFFICIENT_DATA, not OK")
}

func TestGPUHealthcheckStatusTransition(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Pin the clock near the later of the two file timestamps so both reads
	// (00:00:00 and 00:01:00) fall inside gpuMetricsMaxStaleness.
	pinClockNear(t, "2026-01-01T00:01:00Z")
	err := os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","healthy":true,"gpus":[]}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMHandler(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, status)

	// Transition to unhealthy
	err = os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:01:00Z","healthy":false,"unhealthy_reason":"XID_79","gpus":[]}`), 0644)
	require.NoError(t, err)

	status = hc.RunCheck()
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusImpaired, status)
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, hc.GetLastHealthcheckStatus())
}
