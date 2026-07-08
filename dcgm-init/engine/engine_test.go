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

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/gpu/dcgm"
	mock_dcgm "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/dcgm/mocks"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngine(client dcgm.Client, outputPath string, collectionFreq time.Duration, oneShot bool) *Engine {
	return &Engine{
		client:         client,
		outputPath:     outputPath,
		collectionFreq: collectionFreq,
		oneShot:        oneShot,
	}
}

func TestNewClampsNonPositiveInterval(t *testing.T) {
	// A non-positive interval would panic time.NewTicker; New() must clamp it
	// to the default so the "start" command cannot be crashed by a bad flag.
	for _, freq := range []time.Duration{0, -1 * time.Second} {
		eng := New("", "/tmp/does-not-matter.json", freq, false)
		assert.Equal(t, DefaultCollectionFreq, eng.collectionFreq,
			"non-positive interval %s should be clamped to the default", freq)
	}

	// A positive interval is preserved as-is.
	eng := New("", "/tmp/does-not-matter.json", 5*time.Second, false)
	assert.Equal(t, 5*time.Second, eng.collectionFreq)
}

func TestRunExitsOnContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := eng.run(ctx)

	assert.NoError(t, err, "run() should return nil on context cancellation")
}

func TestRunOneShotCollectsAndExits(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, true)

	ctx := context.Background()

	err := eng.run(ctx)

	assert.NoError(t, err, "run() in one-shot mode should complete without error")

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)
	assert.Len(t, output.GPUs, 1)
	assert.Equal(t, "GPU-test-001", output.GPUs[0].GPUUUID)
}

func TestRunReconcileFailureReturnsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(false, assert.AnError).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	ctx := context.Background()

	err := eng.run(ctx)

	assert.Error(t, err, "run() should return error when initial reconciliation fails")
	assert.Contains(t, err.Error(), "initial DCGM reconciliation failed")
}

// TestStartUnusableOutputDirReturnsErrOutputDirUnusable verifies that a bad
// output path (a config problem a restart cannot fix) is reported wrapping
// ErrOutputDirUnusable, so main() maps it to RestartPreventExitCode and systemd
// will not restart-loop.
func TestStartUnusableOutputDirReturnsErrOutputDirUnusable(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Point the output under a regular file so MkdirAll cannot create the dir.
	tmpDir := t.TempDir()
	notADir := filepath.Join(tmpDir, "iamafile")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0644))
	outputPath := filepath.Join(notADir, "sub", "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// Shutdown runs via the deferred cleanup even on the early return.
	mockClient.EXPECT().Shutdown().Return(nil).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.Start()
	require.Error(t, err, "Start() should fail when the output directory cannot be created")
	assert.ErrorIs(t, err, ErrOutputDirUnusable, "output-dir failure should wrap ErrOutputDirUnusable")
}

// TestStartCancelsRunLoopOnSIGTERM verifies the shutdown mechanism the systemd
// unit relies on now that there is no "stop" command / ExecStop: sending SIGTERM
// to the process cancels the run loop and Start() returns nil after shutting the
// client down exactly once.
func TestStartCancelsRunLoopOnSIGTERM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()
	mockClient.EXPECT().Shutdown().Return(nil).Times(1)

	// Long interval so the loop stays blocked in select until the signal fires.
	eng := newTestEngine(mockClient, outputPath, time.Hour, false)

	done := make(chan error, 1)
	go func() {
		done <- eng.Start()
	}()

	// Wait until the run loop is active (the output file has been written once),
	// then deliver SIGTERM to this process — Start()'s watcher should cancel the
	// context and return.
	require.Eventually(t, func() bool {
		_, err := os.Stat(outputPath)
		return err == nil
	}, 2*time.Second, 5*time.Millisecond, "Start() should begin collecting before shutdown")

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case err := <-done:
		assert.NoError(t, err, "Start() should return nil after SIGTERM cancels the run loop")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after SIGTERM was delivered")
	}
}

// TestStartOneShotWritesFileAndShutsDown exercises Start() end-to-end in
// one-shot mode: it must create the output directory, collect once, write the
// file, and shut the client down exactly once (covering the signal-handler /
// MkdirAll / deferred-Shutdown paths that only run inside Start()).
func TestStartOneShotWritesFileAndShutsDown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Nested path so we also cover MkdirAll creating a missing parent dir.
	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "nested", "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-start-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()
	// Shutdown must be called exactly once via the deferred cleanup.
	mockClient.EXPECT().Shutdown().Return(nil).Times(1)

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, true)

	err := eng.Start()
	require.NoError(t, err, "Start() in one-shot mode should complete without error")

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(data, &output))
	require.Len(t, output.GPUs, 1)
	assert.Equal(t, "GPU-start-001", output.GPUs[0].GPUUUID)
}

func TestCollectAndWriteCreatesValidJSON(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	utilization := 85.0
	memUtil := 50.0
	memTotal := uint64(16106127360)
	memUsed := uint64(8053063680)
	power := 250.5
	temp := 72.0

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{
			GPUUUID:            "GPU-abc-123",
			GPUUtilization:     &utilization,
			MemoryUtilization:  &memUtil,
			MemoryTotal:        &memTotal,
			MemoryUsed:         &memUsed,
			PowerDraw:          &power,
			Temperature:        &temp,
			RestartAppXidCount: 0,
		},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)

	assert.NotEmpty(t, output.Timestamp)
	require.Len(t, output.GPUs, 1)
	assert.Equal(t, "GPU-abc-123", output.GPUs[0].GPUUUID)
	assert.Equal(t, 85.0, *output.GPUs[0].GPUUtilization)
	assert.Equal(t, 50.0, *output.GPUs[0].MemoryUtilization)
	assert.Equal(t, uint64(16106127360), *output.GPUs[0].MemoryTotal)
	assert.Equal(t, uint64(8053063680), *output.GPUs[0].MemoryUsed)
	assert.Equal(t, 250.5, *output.GPUs[0].PowerDraw)
	assert.Equal(t, 72.0, *output.GPUs[0].Temperature)
	assert.Equal(t, int64(0), output.GPUs[0].RestartAppXidCount)
}

func TestCollectAndWriteAtomicRename(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	// Final file should exist
	_, err = os.Stat(outputPath)
	assert.NoError(t, err, "Output file should exist after collectAndWrite")

	// Temp file should NOT exist (renamed away)
	_, err = os.Stat(outputPath + ".tmp")
	assert.True(t, os.IsNotExist(err), "Temp file should not exist after rename")
}

func TestCollectAndWriteReportsHealthyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-healthy-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)

	assert.True(t, output.Healthy, "Healthy GPU should report healthy=true")
	assert.Empty(t, output.UnhealthyReason, "Healthy GPU should have no unhealthy reason")
	assert.False(t, output.ConnectionLost, "Healthy GPU should not report connection lost")
}

func TestCollectAndWriteReportsUnhealthyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-unhealthy-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("XID_48").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)

	assert.False(t, output.Healthy, "Unhealthy GPU should report healthy=false")
	assert.Equal(t, "XID_48", output.UnhealthyReason, "Should report the XID error code")
}

// TestCollectAndWriteReportsConnectionLost verifies that when the DCGM
// connection is lost, the shared file records connection_lost=true so the
// reader can report INSUFFICIENT_DATA rather than trusting Healthy (which the
// client returns true while disconnected).
func TestCollectAndWriteReportsConnectionLost(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{}, nil).AnyTimes()
	// IsHealthy returns true while disconnected by design; ConnectionLost is
	// what disambiguates the UNKNOWN state.
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(true).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(data, &output))

	assert.True(t, output.ConnectionLost, "Should report connection_lost=true when the DCGM connection is lost")
}

// TestCollectAndWriteWritesStatusOnGetMetricsFailure verifies that a failure to
// collect per-GPU metrics is non-fatal: collectAndWrite still writes a fresh
// status snapshot (with the current health/connection state and an empty GPU
// list) rather than returning an error and leaving the file stale/absent.
func TestCollectAndWriteWritesStatusOnGetMetricsFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return(nil, assert.AnError).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(true).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err, "collectAndWrite should not fail when GetMetrics fails")

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err, "a status file should still be written when GetMetrics fails")

	var output metricsOutput
	require.NoError(t, json.Unmarshal(data, &output))

	assert.NotEmpty(t, output.Timestamp, "status file should carry a fresh timestamp")
	assert.True(t, output.ConnectionLost, "status file should reflect connection lost")
	assert.Empty(t, output.GPUs, "no per-GPU metrics should be present on collection failure")
}

func TestCollectAndWriteReportsUnhealthyWhenNotInitialized(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-disconnected-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	err = json.Unmarshal(data, &output)
	require.NoError(t, err)

	assert.False(t, output.Healthy, "Should report unhealthy when client is not healthy")
}
