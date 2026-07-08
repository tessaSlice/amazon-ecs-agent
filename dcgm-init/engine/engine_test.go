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

package engine

import (
	"context"
	"encoding/json"
	"os"
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

// newTestEngine builds an Engine around a (mock) client. Unlike the production
// New(), it takes the client directly so tests can inject a mock. outputPath and
// collectionFreq are package consts, so tests redirect I/O via the
// osWriteFile / osRename / osStat / ... seams rather than through per-engine
// fields.
func newTestEngine(client dcgm.Client) *Engine {
	return &Engine{client: client}
}

// captureWrites redirects the filesystem seams so collectAndWrite operates on an
// in-memory buffer instead of the fixed outputPath const, and marks the output
// directory as an existing writable directory. It returns a func yielding the
// bytes last "renamed" into place (i.e. the committed file contents) and
// restores the real seams via t.Cleanup.
//
// The shared state is guarded by a mutex because TestStartCancelsRunLoopOnSIGTERM
// runs Start() (which writes via the seams) on one goroutine while the test
// polls the returned accessor on another.
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

	var mu sync.Mutex
	staging := map[string][]byte{}
	var committed []byte
	osWriteFile = func(name string, data []byte, _ os.FileMode) error {
		mu.Lock()
		defer mu.Unlock()
		staging[name] = append([]byte(nil), data...)
		return nil
	}
	osRename = func(oldpath, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		committed = staging[oldpath]
		delete(staging, oldpath)
		return nil
	}
	osRemove = func(name string) error {
		mu.Lock()
		defer mu.Unlock()
		delete(staging, name)
		return nil
	}

	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return committed
	}
}

// dirFileInfo returns a real os.FileInfo for a directory (t.TempDir), used to
// satisfy the info.IsDir() check in ensureOutputDir without touching outputPath.
func dirFileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	return fi
}

func TestRunExitsOnContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := eng.run(ctx)

	assert.NoError(t, err, "run() should return nil on context cancellation")
}

func TestRunReconcileFailureReturnsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(false, assert.AnError).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.run(context.Background())

	assert.Error(t, err, "run() should return error when initial reconciliation fails")
	assert.Contains(t, err.Error(), "initial DCGM reconciliation failed")
}

// TestStartUnusableOutputDirReturnsErrOutputDirUnusable verifies that a bad
// output directory (a config problem a restart cannot fix) surfaces through
// Start() wrapping ErrOutputDirUnusable, so main() maps it to
// RestartPreventExitCode and systemd will not restart-loop. It injects an
// osStat that reports the dir does not exist and an osMkdirAll that fails, i.e.
// it exercises the create-fallback branch of ensureOutputDir. The other
// branches are covered directly by TestEnsureOutputDir below.
func TestStartUnusableOutputDirReturnsErrOutputDirUnusable(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	origStat, origMkdir := osStat, osMkdirAll
	defer func() { osStat, osMkdirAll = origStat, origMkdir }()
	osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	osMkdirAll = func(string, os.FileMode) error { return syscall.EACCES }

	mockClient := mock_dcgm.NewMockClient(ctrl)
	// Shutdown runs via the deferred cleanup even on the early return.
	mockClient.EXPECT().Shutdown().Return(nil).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.Start()
	require.Error(t, err, "Start() should fail when the output directory is unusable")
	assert.ErrorIs(t, err, ErrOutputDirUnusable, "output-dir failure should wrap ErrOutputDirUnusable")
	assert.ErrorIs(t, err, syscall.EACCES, "output-dir failure should keep the underlying OS cause")
}

// TestEnsureOutputDir covers every branch of ensureOutputDir deterministically
// by injecting the filesystem seams (osStat/checkAccess/osMkdirAll). Each
// failure branch must wrap both ErrOutputDirUnusable (for the exit-code mapping)
// and the underlying OS cause (so callers can errors.Is the specific reason).
func TestEnsureOutputDir(t *testing.T) {
	// Restore the real implementations after the test.
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
			name:      "path exists but is not a directory",
			stat:      func(string) (os.FileInfo, error) { return fakeFileInfo{isDir: false}, nil },
			wantErr:   true,
			wantCause: nil, // no OS cause for this branch
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

			eng := &Engine{}
			err := eng.ensureOutputDir()

			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrOutputDirUnusable, "must wrap ErrOutputDirUnusable for the exit-code mapping")
			if tc.wantCause != nil {
				assert.ErrorIs(t, err, tc.wantCause, "must keep the underlying OS cause in the error chain")
			}
		})
	}
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

// TestStartCancelsRunLoopOnSIGTERM verifies the shutdown mechanism the systemd
// unit relies on now that there is no "stop" command / ExecStop: sending SIGTERM
// to the process cancels the run loop and Start() returns nil after shutting the
// client down exactly once.
func TestStartCancelsRunLoopOnSIGTERM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()
	mockClient.EXPECT().Shutdown().Return(nil).Times(1)

	eng := newTestEngine(mockClient)

	done := make(chan error, 1)
	go func() {
		done <- eng.Start()
	}()

	// Wait until the run loop is active (the output file has been written once),
	// then deliver SIGTERM to this process — Start()'s watcher should cancel the
	// context and return.
	require.Eventually(t, func() bool {
		return committed() != nil
	}, 2*time.Second, 5*time.Millisecond, "Start() should begin collecting before shutdown")

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case err := <-done:
		assert.NoError(t, err, "Start() should return nil after SIGTERM cancels the run loop")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after SIGTERM was delivered")
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

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

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

// TestCollectAndWriteAtomicRename verifies collectAndWrite stages to a .tmp path
// and renames it onto the final outputPath (never leaving the temp file behind).
func TestCollectAndWriteAtomicRename(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Restore seams after the test.
	origStat, origAccess, origMkdir := osStat, checkAccess, osMkdirAll
	origWrite, origRename, origRemove := osWriteFile, osRename, osRemove
	defer func() {
		osStat, checkAccess, osMkdirAll = origStat, origAccess, origMkdir
		osWriteFile, osRename, osRemove = origWrite, origRename, origRemove
	}()
	osStat = func(string) (os.FileInfo, error) { return dirFileInfo(t), nil }
	checkAccess = func(string, uint32) error { return nil }

	staging := map[string][]byte{}
	renamed := map[string]bool{}
	osWriteFile = func(name string, data []byte, _ os.FileMode) error {
		staging[name] = data
		return nil
	}
	osRename = func(oldpath, newpath string) error {
		delete(staging, oldpath)
		renamed[newpath] = true
		return nil
	}

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-test-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	assert.True(t, renamed[outputPath], "final outputPath should be produced via rename")
	assert.NotContains(t, staging, outputPath+".tmp", "temp file should not remain after rename")
}

func TestCollectAndWriteReportsHealthyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-healthy-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

	assert.True(t, output.Healthy, "Healthy GPU should report healthy=true")
	assert.Empty(t, output.UnhealthyReason, "Healthy GPU should have no unhealthy reason")
	assert.False(t, output.ConnectionLost, "Healthy GPU should not report connection lost")
}

func TestCollectAndWriteReportsUnhealthyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-unhealthy-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("XID_48").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

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
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{}, nil).AnyTimes()
	// IsHealthy returns true while disconnected by design; ConnectionLost is
	// what disambiguates the UNKNOWN state.
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(true).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

	assert.True(t, output.ConnectionLost, "Should report connection_lost=true when the DCGM connection is lost")
}

// TestCollectAndWriteWritesStatusOnGetMetricsFailure verifies that a failure to
// collect per-GPU metrics is non-fatal: collectAndWrite still writes a fresh
// status snapshot (with the current health/connection state and an empty GPU
// list) rather than returning an error and leaving the file stale/absent.
func TestCollectAndWriteWritesStatusOnGetMetricsFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return(nil, assert.AnError).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(true).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(true).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err, "collectAndWrite should not fail when GetMetrics fails")

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

	assert.NotEmpty(t, output.Timestamp, "status file should carry a fresh timestamp")
	assert.True(t, output.ConnectionLost, "status file should reflect connection lost")
	assert.Empty(t, output.GPUs, "no per-GPU metrics should be present on collection failure")
}

func TestCollectAndWriteReportsUnhealthyWhenNotInitialized(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	committed := captureWrites(t)

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-disconnected-001"},
	}, nil).AnyTimes()
	mockClient.EXPECT().IsHealthy().Return(false).AnyTimes()
	mockClient.EXPECT().UnhealthyReason().Return("").AnyTimes()
	mockClient.EXPECT().IsConnectionLost().Return(false).AnyTimes()

	eng := newTestEngine(mockClient)

	err := eng.collectAndWrite(context.Background())
	require.NoError(t, err)

	var output metricsOutput
	require.NoError(t, json.Unmarshal(committed(), &output))

	assert.False(t, output.Healthy, "Should report unhealthy when client is not healthy")
}
