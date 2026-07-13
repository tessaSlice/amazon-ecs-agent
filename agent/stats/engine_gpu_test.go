//go:build unit && linux

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package stats

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/amazon-ecs-agent/agent/gpu"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gpuMetricsEmitted drains one telemetry message (an idle engine always sends
// one per publishMetrics tick) and reports whether it carried instance-level GPU
// metrics. Emission is the observable outcome of a fresh per-tick snapshot, so
// tests assert on it rather than on the engine's internal fields.
func gpuMetricsEmitted(t *testing.T, ch <-chan ecstcs.TelemetryMessage) bool {
	t.Helper()
	select {
	case msg := <-ch:
		return msg.InstanceMetrics != nil && len(msg.InstanceMetrics.GeneralMetricsPayload) > 0
	default:
		t.Fatal("expected a telemetry message to be published")
		return false
	}
}

func TestGPUMetricsNotEmittedWhenTimestampStale(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write initial GPU metrics file
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 85.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := NewDockerStatsEngine(&cfg, nil, nil, telemetryMessages, healthMessages, nil)
	var cancel context.CancelFunc
	engine.ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	// Simulate 3 ticks to trigger GPU emission (gpuMetricsPublishCount >= 3)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	// First emission should carry instance GPU metrics (new timestamp) and
	// advance the de-dup cursor once the message is sent.
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "First tick with new timestamp should emit GPU metrics")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp)

	// Simulate 3 more ticks with the SAME file (timestamp unchanged)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	// Second emission should NOT carry GPU metrics (stale timestamp)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Second tick with same timestamp should not emit GPU metrics")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp, "Timestamp should not change")
}

func TestGPUMetricsEmittedWhenTimestampChanges(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write initial GPU metrics
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 50.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := NewDockerStatsEngine(&cfg, nil, nil, telemetryMessages, healthMessages, nil)
	var cancel context.CancelFunc
	engine.ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	// First emission
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages))
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp)

	// Same timestamp — no emission
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages))

	// Update file with new timestamp (simulates dcgm-init writing next tick)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:01:00Z", 99.0)

	// New timestamp — should emit again
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "New timestamp should trigger emission")
	assert.Equal(t, "2026-01-01T00:01:00Z", engine.lastGPUTimestamp)
}

func TestGPUMetricsNotEmittedBeforeThirdTick(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 75.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := NewDockerStatsEngine(&cfg, nil, nil, telemetryMessages, healthMessages, nil)
	var cancel context.CancelFunc
	engine.ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	// First tick (count goes to 1) — should not emit
	engine.gpuMetricsPublishCount = 0
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Should not emit on first tick")

	// Second tick (count goes to 2) — should not emit
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Should not emit on second tick")

	// Third tick (count goes to 3, resets to 0) — should emit
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "Should emit on third tick")
}

func TestGPUMetricsNotEmittedWhenTimestampGoesBackward(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write metrics with a recent timestamp
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:05:00Z", 90.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := NewDockerStatsEngine(&cfg, nil, nil, telemetryMessages, healthMessages, nil)
	var cancel context.CancelFunc
	engine.ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	// First emission succeeds (new timestamp)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages))
	assert.Equal(t, "2026-01-01T00:05:00Z", engine.lastGPUTimestamp)

	// Write metrics with an OLDER timestamp (clock skew, file corruption, etc.)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:03:00Z", 10.0)

	// Should NOT emit because timestamp is older than lastGPUTimestamp
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should not emit metrics when file timestamp is older than last emitted timestamp")
	assert.Equal(t, "2026-01-01T00:05:00Z", engine.lastGPUTimestamp,
		"lastGPUTimestamp should remain at the newer value")

	// Write metrics with the same timestamp as the last emitted (equal, not greater)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:05:00Z", 50.0)

	// Should NOT emit because timestamp is equal (not strictly greater)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should not emit metrics when file timestamp equals last emitted timestamp")

	// Write metrics with a newer timestamp — should emit
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:06:00Z", 100.0)

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should emit metrics when file timestamp is strictly newer")
	assert.Equal(t, "2026-01-01T00:06:00Z", engine.lastGPUTimestamp)
}

func writeGPUMetricsFile(t *testing.T, path string, timestamp string, utilization float64) {
	t.Helper()
	data := gpu.GPUMetricsFileData{
		Timestamp: timestamp,
		GPUs: []gpu.GPUMetric{
			{
				GPUUUID:        "GPU-test-001",
				GPUUtilization: aws.Float64(utilization),
			},
		},
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	require.NoError(t, err)
	err = os.WriteFile(path, bytes, 0644)
	require.NoError(t, err)
}
