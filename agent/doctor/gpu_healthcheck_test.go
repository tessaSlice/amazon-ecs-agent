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

func TestGPUHealthcheckReportsOkWhenHealthy(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	freshTS := time.Now().UTC().Format(time.RFC3339)
	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "`+freshTS+`",
		"healthy": true,
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, status)
	assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, hc.GetHealthcheckType())
}

func TestGPUHealthcheckReportsImpairedWhenUnhealthy(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	freshTS := time.Now().UTC().Format(time.RFC3339)
	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "`+freshTS+`",
		"healthy": false,
		"unhealthy_reason": "XID_48",
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusImpaired, status)
}

// assertBootGraceThenInsufficientData checks a nil-returning reader stays
// INITIALIZING within the boot grace, then flips to INSUFFICIENT_DATA after it.
func assertBootGraceThenInsufficientData(t *testing.T, handler *gpu.DCGMMetricsReader) {
	t.Helper()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	original := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = original }()

	hc := NewGPUHealthcheck(handler)

	// Within the boot grace window: no data yet is tolerated, status unchanged.
	timeNow = func() time.Time { return base.Add(gpuBootGracePeriod - time.Second) }
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInitializing, hc.RunCheck(),
		"missing GPU data within the boot grace window must not change the status")

	// After the grace window: still no data flips to INSUFFICIENT_DATA.
	timeNow = func() time.Time { return base.Add(gpuBootGracePeriod) }
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, hc.RunCheck(),
		"missing GPU data after the boot grace window must report INSUFFICIENT_DATA")
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileMissing(t *testing.T) {
	handler := gpu.NewDCGMMetricsReader("/nonexistent/gpu-metrics.json")
	assertBootGraceThenInsufficientData(t, handler)
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileCorrupt(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`not valid json{{{`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	assertBootGraceThenInsufficientData(t, handler)
}

func TestGPUHealthcheckReportsInsufficientDataWhenFileEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(""), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	assertBootGraceThenInsufficientData(t, handler)
}

// After a first successful read, data loss reports INSUFFICIENT_DATA immediately
// even within the boot grace: the grace covers only the initial INITIALIZING period.
func TestGPUHealthcheckDataLossWithinGraceReportsInsufficientData(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	base, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	original := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = original }()

	err = os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": true,
		"gpus": [{"gpu_uuid": "GPU-001"}]
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)
	require.Equal(t, ecstcs.InstanceHealthCheckStatusOk, hc.RunCheck())

	// Delete the file and re-check while still inside the boot grace window:
	// the loss of data after a real status must NOT be masked by the grace.
	require.NoError(t, os.Remove(filePath))
	timeNow = func() time.Time { return base.Add(gpuBootGracePeriod - time.Second) }
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, hc.RunCheck(),
		"data loss after a data-derived status must report INSUFFICIENT_DATA even within the boot grace window")
}

// connection_lost=true reports INSUFFICIENT_DATA even when healthy=true, since
// dcgm-init leaves Healthy true while disconnected.
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

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status)
}

// A timestamp older than the staleness threshold reports INSUFFICIENT_DATA; a
// future one is treated as fresh.
func TestGPUHealthcheckTimestampStaleness(t *testing.T) {
	t.Run("stale timestamp reports INSUFFICIENT_DATA", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "gpu-metrics.json")
		contents := `{"timestamp":"2000-01-01T00:00:00Z","healthy":true,"gpus":[]}`
		require.NoError(t, os.WriteFile(filePath, []byte(contents), 0644))

		hc := NewGPUHealthcheck(gpu.NewDCGMMetricsReader(filePath))
		assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, hc.RunCheck())
	})

	t.Run("future timestamp is not stale and reports OK", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "gpu-metrics.json")
		contents := `{"timestamp":"2099-01-01T00:00:00Z","healthy":true,"gpus":[]}`
		require.NoError(t, os.WriteFile(filePath, []byte(contents), 0644))

		hc := NewGPUHealthcheck(gpu.NewDCGMMetricsReader(filePath))
		assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, hc.RunCheck())
	})
}

// connection_lost=true takes precedence over healthy=false: RunCheck returns
// INSUFFICIENT_DATA, not IMPAIRED.
func TestGPUHealthcheckConnectionLostBeatsUnhealthy(t *testing.T) {
	// Anchor timeNow near the file's timestamp so it is not stale.
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	original := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = original }()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`{
		"timestamp": "2026-01-01T00:00:00Z",
		"healthy": false,
		"unhealthy_reason": "XID_48",
		"connection_lost": true,
		"gpus": []
	}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)
	status := hc.RunCheck()

	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, status,
		"connection_lost must take precedence over an unhealthy report (INSUFFICIENT_DATA, not IMPAIRED)")
}

func TestGPUHealthcheckStatusTransition(t *testing.T) {
	// Anchor timeNow to a fixed point so the file timestamps are within
	// the staleness threshold.
	base := time.Date(2026, 1, 1, 0, 1, 30, 0, time.UTC)
	original := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = original }()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","healthy":true,"gpus":[]}`), 0644)
	require.NoError(t, err)

	handler := gpu.NewDCGMMetricsReader(filePath)
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

// TestGPUHealthcheckRecoversFromInsufficientData verifies the check is not
// latched: after INSUFFICIENT_DATA, a fresh healthy sample flips it back to OK.
func TestGPUHealthcheckRecoversFromInsufficientData(t *testing.T) {
	// Anchor timeNow so the file timestamps are within the staleness threshold.
	base := time.Date(2026, 1, 1, 0, 1, 30, 0, time.UTC)
	original := timeNow
	timeNow = func() time.Time { return base }
	defer func() { timeNow = original }()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	handler := gpu.NewDCGMMetricsReader(filePath)
	hc := NewGPUHealthcheck(handler)

	// First: connection lost -> INSUFFICIENT_DATA.
	err := os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","healthy":true,"connection_lost":true,"gpus":[]}`), 0644)
	require.NoError(t, err)
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, hc.RunCheck())

	// Then: a fresh, connected, healthy sample must recover to OK.
	err = os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:01:00Z","healthy":true,"gpus":[]}`), 0644)
	require.NoError(t, err)
	status := hc.RunCheck()
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk, status)
	assert.Equal(t, ecstcs.InstanceHealthCheckStatusInsufficientData, hc.GetLastHealthcheckStatus())
}
