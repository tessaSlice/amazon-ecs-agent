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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
		signalProcess:  syscall.Kill,
	}
}

func TestNewClampsNonPositiveInterval(t *testing.T) {
	// A non-positive interval would panic time.NewTicker; New() must clamp it
	// to the default so the "start" command cannot be crashed by a bad flag.
	for _, freq := range []time.Duration{0, -1 * time.Second} {
		eng := New("", "/tmp/does-not-matter.json", "", freq, false)
		assert.Equal(t, DefaultCollectionFreq, eng.collectionFreq,
			"non-positive interval %s should be clamped to the default", freq)
	}

	// A positive interval is preserved as-is.
	eng := New("", "/tmp/does-not-matter.json", "", 5*time.Second, false)
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

func TestStopReturnsNil(t *testing.T) {
	eng := &Engine{}
	err := eng.Stop()
	assert.NoError(t, err, "Stop() should be a no-op and return nil when no run loop is active")
}

// TestTerminalErrorUnwraps verifies *TerminalError wraps its cause so
// errors.As/errors.Is can classify it (the basis for the exit-5 mapping).
func TestTerminalErrorUnwraps(t *testing.T) {
	cause := assert.AnError
	var te error = NewTerminalError(cause)

	var got *TerminalError
	assert.True(t, errors.As(te, &got), "errors.As should recognize *TerminalError")
	assert.ErrorIs(t, te, cause, "TerminalError should unwrap to its cause")
	assert.Equal(t, cause.Error(), te.Error(), "Error() should surface the cause message")
}

// TestStartUnwritableOutputDirReturnsTerminalError verifies that a bad output
// path (a config problem a restart cannot fix) is reported as a *TerminalError,
// so main() maps it to TerminalFailureExitCode and systemd will not restart-loop.
func TestStartUnwritableOutputDirReturnsTerminalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Point the output under a regular file so MkdirAll cannot create the dir.
	tmpDir := t.TempDir()
	notADir := filepath.Join(tmpDir, "iamafile")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0644))
	outputPath := filepath.Join(notADir, "sub", "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// Shutdown runs via the deferred cleanup even on the early terminal return.
	mockClient.EXPECT().Shutdown().Return(nil).AnyTimes()

	eng := newTestEngine(mockClient, outputPath, 60*time.Second, false)
	// No pid path so the failure is specifically the output dir.
	eng.pidPath = ""

	err := eng.Start()
	require.Error(t, err, "Start() should fail when the output directory cannot be created")

	var te *TerminalError
	assert.True(t, errors.As(err, &te), "output-dir failure should be a *TerminalError")
}

// TestStopCancelsRunningStart verifies that, when Start() and Stop() run in the
// same process, Stop() cancels the run() context and causes Start() to return.
// (Under systemd, start/stop are separate processes and Stop() is a no-op; this
// covers the in-process path.)
func TestStopCancelsRunningStart(t *testing.T) {
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

	// Long interval so the loop stays blocked in select until Stop() cancels it.
	eng := newTestEngine(mockClient, outputPath, time.Hour, false)

	done := make(chan error, 1)
	go func() {
		done <- eng.Start()
	}()

	// Wait until Start() has published its cancel func (run loop is active),
	// then request stop.
	require.Eventually(t, func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.cancel != nil
	}, 2*time.Second, 5*time.Millisecond, "Start() should publish a cancel func")

	assert.NoError(t, eng.Stop(), "Stop() should cancel the run loop and return nil")

	select {
	case err := <-done:
		assert.NoError(t, err, "Start() should return nil after Stop() cancels the context")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop() was called")
	}

	// After Start() returns, cancel must be cleared so a later Stop() is a no-op.
	eng.mu.Lock()
	clearedCancel := eng.cancel
	eng.mu.Unlock()
	assert.Nil(t, clearedCancel, "cancel should be cleared after Start() returns")
	assert.NoError(t, eng.Stop(), "Stop() after Start() returns should be a no-op")
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
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid")
	eng.pidPath = pidPath

	err := eng.Start()
	require.NoError(t, err, "Start() in one-shot mode should complete without error")

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(data, &output))
	require.Len(t, output.GPUs, 1)
	assert.Equal(t, "GPU-start-001", output.GPUs[0].GPUUUID)

	// The pid file must be cleaned up when Start() returns.
	_, statErr := os.Stat(pidPath)
	assert.True(t, os.IsNotExist(statErr), "pid file should be removed after Start() returns")
}

// TestStartWritesPidFileWithOwnPid verifies Start() records the current PID in
// the configured pid file while the run loop is active.
func TestStartWritesPidFileWithOwnPid(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	outputPath := filepath.Join(tmpDir, "gpu-metrics.json")
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()
	mockClient.EXPECT().Shutdown().Return(nil).Times(1)

	eng := newTestEngine(mockClient, outputPath, time.Hour, false)
	eng.pidPath = pidPath

	done := make(chan error, 1)
	go func() { done <- eng.Start() }()

	// Once the run loop is active, the pid file should contain our PID.
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid == os.Getpid()
	}, 2*time.Second, 5*time.Millisecond, "pid file should record the current PID")

	require.NoError(t, eng.Stop(), "in-process Stop() should cancel the loop")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}
}

// TestStopSignalsProcessFromPidFile verifies the cross-process path: with no
// in-process run loop, Stop() reads the pid file and signals that PID with
// SIGTERM (the mechanism systemd/the CLI rely on to cancel the running loop).
func TestStopSignalsProcessFromPidFile(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte("4242\n"), 0644))

	var (
		mu        sync.Mutex
		gotPid    int
		gotSig    syscall.Signal
		signalled bool
	)
	eng := &Engine{
		pidPath: pidPath,
		signalProcess: func(pid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			gotPid, gotSig, signalled = pid, sig, true
			return nil
		},
	}

	require.NoError(t, eng.Stop())

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, signalled, "Stop() should signal the process recorded in the pid file")
	assert.Equal(t, 4242, gotPid)
	assert.Equal(t, syscall.SIGTERM, gotSig)
}

// TestStopNoPidFileIsNoop verifies Stop() treats a missing pid file as
// "already stopped" and does not signal anything.
func TestStopNoPidFileIsNoop(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid") // never created

	signalled := false
	eng := &Engine{
		pidPath: pidPath,
		signalProcess: func(pid int, sig syscall.Signal) error {
			signalled = true
			return nil
		},
	}

	assert.NoError(t, eng.Stop(), "Stop() with no pid file should be a no-op")
	assert.False(t, signalled, "Stop() should not signal anything when no pid file exists")
}

// TestStopStalePidFileRemoved verifies that when the recorded process no longer
// exists (signal returns ESRCH), Stop() cleans up the stale pid file and
// reports success.
func TestStopStalePidFileRemoved(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte("999999\n"), 0644))

	eng := &Engine{
		pidPath: pidPath,
		signalProcess: func(pid int, sig syscall.Signal) error {
			return syscall.ESRCH // no such process
		},
	}

	assert.NoError(t, eng.Stop(), "Stop() should treat a stale pid file as already stopped")
	_, statErr := os.Stat(pidPath)
	assert.True(t, os.IsNotExist(statErr), "stale pid file should be removed")
}

// TestStopMalformedPidFileErrors verifies a corrupt pid file surfaces an error
// rather than silently signalling a bogus PID.
func TestStopMalformedPidFileErrors(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "dcgm-init.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte("not-a-pid\n"), 0644))

	signalled := false
	eng := &Engine{
		pidPath: pidPath,
		signalProcess: func(pid int, sig syscall.Signal) error {
			signalled = true
			return nil
		},
	}

	err := eng.Stop()
	assert.Error(t, err, "Stop() should error on a malformed pid file")
	assert.False(t, signalled, "Stop() should not signal on a malformed pid file")
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

	// Staging file should NOT exist (renamed away)
	_, err = os.Stat(outputPath + ".staging")
	assert.True(t, os.IsNotExist(err), "Staging file should not exist after rename")
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
