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

// newTestEngine builds an Engine around a (mock) client. Unlike the production
// New(), it takes the client directly so tests can inject a mock. outputPath and
// collectionFreq are package consts, so tests redirect I/O via the osWriteFile /
// osRename / osStat / ... seams rather than through per-engine fields.
func newTestEngine(client dcgm.Client) *Engine {
	return &Engine{client: client}
}

// captureWrites redirects the filesystem seams so collectAndWrite operates on an
// in-memory buffer instead of the fixed outputPath const, and marks the output
// directory as an existing writable directory. It returns a func yielding the
// bytes last "renamed" into place (i.e. the committed file contents) and
// restores the real seams via t.Cleanup.
func captureWrites(t *testing.T) func() []byte {
	t.Helper()
	origStat, origAccess, origMkdir := osStat, checkAccess, osMkdirAll
	origWrite, origRename, origRemove := osWriteFile, osRename, osRemove
	t.Cleanup(func() {
		osStat, checkAccess, osMkdirAll = origStat, origAccess, origMkdir
		osWriteFile, osRename, osRemove = origWrite, origRename, origRemove
	})

	// Output directory exists and is writable.
	osStat = func(string) (os.FileInfo, error) { return dirFileInfo(t), nil }
	checkAccess = func(string, uint32) error { return nil }
	osMkdirAll = func(string, os.FileMode) error { return nil }

	staging := map[string][]byte{}
	var committed []byte
	osWriteFile = func(name string, data []byte, _ os.FileMode) error {
		staging[name] = append([]byte(nil), data...)
		return nil
	}
	osRename = func(oldpath, newpath string) error {
		committed = staging[oldpath]
		delete(staging, oldpath)
		return nil
	}
	osRemove = func(name string) error { delete(staging, name); return nil }

	return func() []byte { return committed }
}

// dirFileInfo returns a real os.FileInfo for a directory (t.TempDir), used to
// satisfy the info.IsDir() check in ensureOutputDir without hitting outputPath.
func dirFileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	return fi
}

// metricsClient wires up a mock DCGM client whose GetMetrics returns the given
// GPUs for the whole test. The engine no longer consults health/connection
// state, so only GetMetrics is stubbed here.
func metricsClient(ctrl *gomock.Controller, gpus []gputypes.GPUMetric) *mock_dcgm.MockClient {
	c := mock_dcgm.NewMockClient(ctrl)
	c.EXPECT().GetMetrics(gomock.Any()).Return(gpus, nil).AnyTimes()
	return c
}

func TestNewBuildsEngineWithClient(t *testing.T) {
	eng := New()
	require.NotNil(t, eng)
	assert.NotNil(t, eng.client, "New() should construct a DCGM client")
}

func TestRunExitsOnContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	captureWrites(t)

	mockClient := metricsClient(ctrl, []gputypes.GPUMetric{{GPUUUID: "GPU-test-001"}})
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	eng := newTestEngine(mockClient)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := eng.run(ctx)
	assert.NoError(t, err, "run() should return nil on context cancellation")
}

func TestRunReconcileFailureReturnsErrSetup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(false, assert.AnError).Times(1)

	eng := newTestEngine(mockClient)

	err := eng.run(context.Background())
	require.Error(t, err, "run() should return an error when initial reconciliation fails")
	assert.ErrorIs(t, err, ErrSetup, "initial reconcile failure should wrap ErrSetup for the exit-code mapping")
	assert.Contains(t, err.Error(), "initial DCGM reconciliation failed")
}

// TestRunLoopEscalatesPersistentReconcileFailures verifies the reconcile
// failure-escalation this branch adds: once the tick-loop Reconcile fails
// maxConsecutiveFailures times in a row, runLoop returns an error (so the
// process exits and systemd restarts) rather than logging forever. The tick
// channel is driven manually so no real time passes.
func TestRunLoopEscalatesPersistentReconcileFailures(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// Every tick-loop reconcile fails (runLoop does not call the initial
	// Reconcile — run() does — so all reconcile calls here are loop ticks).
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(false, assert.AnError).AnyTimes()
	// Metrics may still be queried on each tick; keep them succeeding so only the
	// reconcile counter escalates.
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{}, nil).AnyTimes()

	eng := newTestEngine(mockClient)
	err := eng.runLoop(context.Background(), fireTicks(maxConsecutiveFailures))

	require.Error(t, err, "runLoop should give up after persistent reconcile failures")
	assert.Contains(t, err.Error(), "consecutive DCGM reconciliation failures")
}

// TestRunLoopEscalatesPersistentWriteFailures verifies the write
// failure-escalation: once collectAndWrite fails maxConsecutiveFailures times in
// a row (e.g. a read-only filesystem), runLoop returns an error instead of
// looping forever.
func TestRunLoopEscalatesPersistentWriteFailures(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Directory looks fine; the write itself always fails.
	origStat, origAccess, origMkdir := osStat, checkAccess, osMkdirAll
	origWrite := osWriteFile
	t.Cleanup(func() {
		osStat, checkAccess, osMkdirAll = origStat, origAccess, origMkdir
		osWriteFile = origWrite
	})
	osStat = func(string) (os.FileInfo, error) { return dirFileInfo(t), nil }
	checkAccess = func(string, uint32) error { return nil }
	osMkdirAll = func(string, os.FileMode) error { return nil }
	osWriteFile = func(string, []byte, os.FileMode) error { return syscall.EROFS }

	mockClient := metricsClient(ctrl, []gputypes.GPUMetric{{GPUUUID: "GPU-test-001"}})
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()

	eng := newTestEngine(mockClient)
	// The initial collection (before the first tick) is one write failure, so
	// maxConsecutiveFailures-1 more ticks reach the threshold.
	err := eng.runLoop(context.Background(), fireTicks(maxConsecutiveFailures-1))

	require.Error(t, err, "runLoop should give up after persistent write failures")
	assert.Contains(t, err.Error(), "consecutive metrics write failures")
}

// fireTicks returns a tick channel pre-loaded with n ticks. runLoop consumes one
// per iteration; escalation is expected to fire before the channel drains, and
// any surplus ticks are harmless. The channel is buffered so sending never
// blocks the test.
func fireTicks(n int) <-chan time.Time {
	ch := make(chan time.Time, n)
	for i := 0; i < n; i++ {
		ch <- time.Time{}
	}
	return ch
}

// TestGetMetricsHonorsContextCancellation verifies that getMetrics returns
// promptly when ctx is cancelled even if the underlying client call is still
// blocked, so a wedged nv-hostengine cannot stall signal-driven shutdown.
func TestGetMetricsHonorsContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	release := make(chan struct{})
	mockClient := mock_dcgm.NewMockClient(ctrl)
	// GetMetrics blocks until released, simulating a wedged cgo call.
	mockClient.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(
		func(context.Context) ([]gputypes.GPUMetric, error) {
			<-release
			return nil, nil
		}).AnyTimes()
	defer close(release)

	eng := newTestEngine(mockClient)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan error, 1)
	go func() {
		_, err := eng.getMetrics(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled, "getMetrics should return ctx.Err() when cancelled")
	case <-time.After(time.Second):
		t.Fatal("getMetrics did not return promptly on context cancellation")
	}
}

// TestStartUnusableOutputDirReturnsErrSetup verifies that a bad output path (a
// config problem a restart cannot fix) surfaces through Start() wrapping
// ErrSetup, so main() maps it to ExitSetupError. It forces the stat branch to
// report a non-directory.
func TestStartUnusableOutputDirReturnsErrSetup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	origStat := osStat
	t.Cleanup(func() { osStat = origStat })
	osStat = func(string) (os.FileInfo, error) { return fakeFileInfo{isDir: false}, nil }

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// Shutdown runs via the deferred cleanup even on the early return.
	mockClient.EXPECT().Shutdown().Return(nil).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.Start()
	require.Error(t, err, "Start() should fail when the output directory is unusable")
	assert.ErrorIs(t, err, ErrSetup, "output-dir failure should wrap ErrSetup")
}

// TestEnsureOutputDir covers every branch of ensureOutputDir deterministically
// by injecting the filesystem seams. Each failure branch must wrap ErrSetup (for
// the exit-code mapping) and, where applicable, the underlying OS cause.
func TestEnsureOutputDir(t *testing.T) {
	origStat, origAccess, origMkdir := osStat, checkAccess, osMkdirAll
	defer func() { osStat, checkAccess, osMkdirAll = origStat, origAccess, origMkdir }()

	dirInfo := dirFileInfo(t)

	tests := []struct {
		name      string
		stat      func(string) (os.FileInfo, error)
		access    func(string, uint32) error
		mkdir     func(string, os.FileMode) error
		wantErr   bool
		wantCause error // underlying OS cause that must remain in the chain, or nil
	}{
		{
			name:   "existing writable dir succeeds",
			stat:   func(string) (os.FileInfo, error) { return dirInfo, nil },
			access: func(string, uint32) error { return nil },
		},
		{
			name:    "path exists but is not a directory",
			stat:    func(string) (os.FileInfo, error) { return fakeFileInfo{isDir: false}, nil },
			wantErr: true,
		},
		{
			name:      "existing dir not writable",
			stat:      func(string) (os.FileInfo, error) { return dirInfo, nil },
			access:    func(string, uint32) error { return syscall.EACCES },
			wantErr:   true,
			wantCause: syscall.EACCES,
		},
		{
			name:      "stat fails with non-NotExist error",
			stat:      func(string) (os.FileInfo, error) { return nil, syscall.EACCES },
			wantErr:   true,
			wantCause: syscall.EACCES,
		},
		{
			name:      "dir absent and mkdir fails",
			stat:      func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			mkdir:     func(string, os.FileMode) error { return syscall.EACCES },
			wantErr:   true,
			wantCause: syscall.EACCES,
		},
		{
			name:  "dir absent and mkdir succeeds",
			stat:  func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			mkdir: func(string, os.FileMode) error { return nil },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			osStat = tc.stat
			checkAccess = tc.access
			osMkdirAll = tc.mkdir

			err := ensureOutputDir()

			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSetup, "must wrap ErrSetup for the exit-code mapping")
			if tc.wantCause != nil {
				assert.ErrorIs(t, err, tc.wantCause, "must keep the underlying OS cause in the error chain")
			}
		})
	}
}

func TestCollectAndWriteCreatesValidJSON(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	utilization := 85.0
	memUtil := 50.0
	memTotal := uint64(16106127360)
	memUsed := uint64(8053063680)
	power := 250.5
	temp := 72.0

	mockClient := metricsClient(ctrl, []gputypes.GPUMetric{
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
	})
	eng := newTestEngine(mockClient)

	require.NoError(t, eng.collectAndWrite(context.Background()))

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

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

// TestCollectAndWriteAtomicRename verifies the staging file is renamed onto the
// final path (and not left behind) — the atomic-write contract readers rely on.
func TestCollectAndWriteAtomicRename(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	origStat, origAccess, origMkdir := osStat, checkAccess, osMkdirAll
	origWrite, origRename, origRemove := osWriteFile, osRename, osRemove
	t.Cleanup(func() {
		osStat, checkAccess, osMkdirAll = origStat, origAccess, origMkdir
		osWriteFile, osRename, osRemove = origWrite, origRename, origRemove
	})
	osStat = func(string) (os.FileInfo, error) { return dirFileInfo(t), nil }
	checkAccess = func(string, uint32) error { return nil }

	live := map[string]bool{}
	osWriteFile = func(name string, _ []byte, _ os.FileMode) error { live[name] = true; return nil }
	renamed := false
	osRename = func(oldpath, newpath string) error {
		require.Equal(t, outputPath+".tmp", oldpath, "should write to the staging path")
		require.Equal(t, outputPath, newpath, "should rename onto the final path")
		delete(live, oldpath)
		live[newpath] = true
		renamed = true
		return nil
	}
	osRemove = func(name string) error { delete(live, name); return nil }

	mockClient := metricsClient(ctrl, []gputypes.GPUMetric{{GPUUUID: "GPU-test-001"}})
	eng := newTestEngine(mockClient)

	require.NoError(t, eng.collectAndWrite(context.Background()))
	assert.True(t, renamed, "collectAndWrite should rename the staging file onto the final path")
	assert.True(t, live[outputPath], "final file should exist after rename")
	assert.False(t, live[outputPath+".tmp"], "staging file should not remain after rename")
}

// TestCollectAndWriteWipesFileOnDisconnection verifies that when GetMetrics
// fails (DCGM disconnected), collectAndWrite truncates the shared file to empty
// rather than writing stale or partial metrics. The write is non-fatal (nil
// error) so the run loop keeps retrying.
func TestCollectAndWriteWipesFileOnDisconnection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return(nil, assert.AnError).AnyTimes()

	eng := newTestEngine(mockClient)
	require.NoError(t, eng.collectAndWrite(context.Background()), "collectAndWrite should not fail when GetMetrics fails")

	assert.Empty(t, committed(), "the metrics file should be emptied (0 bytes) on disconnection")
}

// TestCollectAndWriteWipesAfterPreviousMetrics verifies that a disconnection
// following a successful collection replaces the previously written metrics with
// an empty file, so a reader never keeps serving stale GPU data.
func TestCollectAndWriteWipesAfterPreviousMetrics(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	gpus := []gputypes.GPUMetric{{GPUUUID: "GPU-was-here"}}
	mockClient := mock_dcgm.NewMockClient(ctrl)
	// First call returns metrics; every later call fails (disconnected).
	gomock.InOrder(
		mockClient.EXPECT().GetMetrics(gomock.Any()).Return(gpus, nil).Times(1),
		mockClient.EXPECT().GetMetrics(gomock.Any()).Return(nil, assert.AnError).AnyTimes(),
	)

	eng := newTestEngine(mockClient)

	// First collection writes real metrics.
	require.NoError(t, eng.collectAndWrite(context.Background()))
	var first metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &first))
	require.Len(t, first.GPUs, 1)

	// Second collection (disconnected) wipes the file.
	require.NoError(t, eng.collectAndWrite(context.Background()))
	assert.Empty(t, committed(), "a disconnection after a good collection should empty the file")
}

// TestCollectAndWriteFailsWhenOutputDirUnusable verifies collectAndWrite
// re-checks the output directory each call and surfaces ErrSetup when it has
// become unusable at runtime (e.g. the tmpfs dir was pruned).
func TestCollectAndWriteFailsWhenOutputDirUnusable(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	origStat, origMkdir := osStat, osMkdirAll
	t.Cleanup(func() { osStat, osMkdirAll = origStat, origMkdir })
	// Directory is gone and cannot be recreated.
	osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	osMkdirAll = func(string, os.FileMode) error { return syscall.EACCES }

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// GetMetrics should not even be reached; allow zero calls.
	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSetup, "a runtime-unusable output dir should wrap ErrSetup")
}

// fakeFileInfo is a minimal os.FileInfo whose IsDir() is controllable, used to
// exercise the "exists but not a directory" branch without touching the real FS.
type fakeFileInfo struct {
	isDir bool
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }
