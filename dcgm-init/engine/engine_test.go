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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mock_dcgm "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/dcgm/mocks"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestEngine builds an Engine wired to a mock client, writing to outputPath
// and ticking at collectionInterval, so tests can redirect writes to a temp
// directory and shrink the ticker instead of touching /var/run/ecs or waiting a
// full production interval.
func newTestEngine(client *mock_dcgm.MockClient, outputPath string, collectionInterval time.Duration) *Engine {
	return &Engine{
		client:             client,
		outputPath:         outputPath,
		collectionInterval: collectionInterval,
	}
}

// expectStatus sets up the health-reporting expectations every collectAndWrite
// invokes. Values are asserted through the written file, not here.
func expectStatus(m *mock_dcgm.MockClient, healthy bool, reason string, connLost bool) {
	m.EXPECT().IsHealthy().Return(healthy).AnyTimes()
	m.EXPECT().UnhealthyReason().Return(reason).AnyTimes()
	m.EXPECT().IsConnectionLost().Return(connLost).AnyTimes()
}

// readOutput reads and unmarshals the metrics file written by the engine.
func readOutput(t *testing.T, path string) dcgmOutput {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out dcgmOutput
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

// TestNew verifies New() constructs an Engine with a live client and the
// production defaults for the output path and collection interval.
func TestNew(t *testing.T) {
	t.Parallel()

	eng, err := New()
	require.NoError(t, err)
	require.NotNil(t, eng)

	assert.NotNil(t, eng.client, "New() should create a DCGM client")
	assert.Equal(t, MetricsFilePath, eng.outputPath)
	assert.Equal(t, metricsCollectionInterval, eng.collectionInterval)
	assert.Equal(t, MetricsFilePath+".tmp", eng.tempPath(), "temp path should be the output path plus .tmp")
}

// TestCollectAndWrite drives collectAndWrite through the same decision points as
// the reference collector's ReconcileAndCollect test, adapted for a file sink:
// a reconcile failure short-circuits before GetMetrics and writes nothing, while
// every path past reconciliation writes a status snapshot (with or without
// per-GPU metrics) that we read back from disk.
func TestCollectAndWrite(t *testing.T) {
	t.Parallel()

	utilization := 75.0

	testCases := []struct {
		name        string
		setupMock   func(*mock_dcgm.MockClient)
		wantWritten bool
		verify      func(t *testing.T, out dcgmOutput)
	}{
		{
			name: "reconcile failure skips GetMetrics and writes nothing",
			setupMock: func(m *mock_dcgm.MockClient) {
				m.EXPECT().Reconcile(gomock.Any()).Return(false, assert.AnError).AnyTimes()
				// GetMetrics must not be reached when reconciliation fails.
				m.EXPECT().GetMetrics(gomock.Any()).Times(0)
			},
			wantWritten: false,
		},
		{
			name: "GetMetrics failure writes status only",
			setupMock: func(m *mock_dcgm.MockClient) {
				m.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).Return(nil, assert.AnError).AnyTimes()
				expectStatus(m, true, "", true)
			},
			wantWritten: true,
			verify: func(t *testing.T, out dcgmOutput) {
				assert.NotEmpty(t, out.Timestamp, "status file should carry a fresh timestamp")
				assert.True(t, out.ConnectionLost, "status file should reflect connection lost")
				assert.Empty(t, out.GPUs, "no per-GPU metrics should be present on collection failure")
			},
		},
		{
			name: "successful collection writes metrics to file",
			setupMock: func(m *mock_dcgm.MockClient) {
				m.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
					{GPUUUID: "GPU-test-uuid-1", GPUUtilization: &utilization},
				}, nil).AnyTimes()
				expectStatus(m, true, "", false)
			},
			wantWritten: true,
			verify: func(t *testing.T, out dcgmOutput) {
				assert.True(t, out.Healthy)
				assert.Empty(t, out.UnhealthyReason)
				assert.False(t, out.ConnectionLost)
				require.Len(t, out.GPUs, 1)
				assert.Equal(t, "GPU-test-uuid-1", out.GPUs[0].GPUUUID)
				require.NotNil(t, out.GPUs[0].GPUUtilization)
				assert.Equal(t, utilization, *out.GPUs[0].GPUUtilization)
			},
		},
		{
			name: "unhealthy GPU records the XID reason",
			setupMock: func(m *mock_dcgm.MockClient) {
				m.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
					{GPUUUID: "GPU-unhealthy-001"},
				}, nil).AnyTimes()
				expectStatus(m, false, "XID_48", false)
			},
			wantWritten: true,
			verify: func(t *testing.T, out dcgmOutput) {
				assert.False(t, out.Healthy, "unhealthy GPU should report healthy=false")
				assert.Equal(t, "XID_48", out.UnhealthyReason, "should report the XID error code")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			outputPath := filepath.Join(t.TempDir(), "gpu-metrics.json")

			mockClient := mock_dcgm.NewMockClient(ctrl)
			tc.setupMock(mockClient)

			eng := newTestEngine(mockClient, outputPath, time.Hour)

			require.NoError(t, eng.collectAndWrite(context.Background()))

			if !tc.wantWritten {
				_, err := os.Stat(outputPath)
				assert.True(t, os.IsNotExist(err), "no file should be written when reconciliation fails")
				return
			}

			// The temp file must have been renamed away, leaving only the final file.
			_, err := os.Stat(eng.tempPath())
			assert.True(t, os.IsNotExist(err), "temp file should not remain after the atomic rename")

			if tc.verify != nil {
				tc.verify(t, readOutput(t, outputPath))
			}
		})
	}
}

// TestRun covers the collection loop the way the reference's PeriodicCollection
// test does: the ticker drives repeated collections, and cancelling the context
// stops the loop cleanly (Start relies on this for signal-driven shutdown).
func TestRun(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		testFunc func(t *testing.T, eng *Engine, mockClient *mock_dcgm.MockClient, getMetricsCalls *atomic.Int64)
	}{
		{
			name: "ticker triggers repeated collection",
			testFunc: func(t *testing.T, eng *Engine, mockClient *mock_dcgm.MockClient, getMetricsCalls *atomic.Int64) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				done := make(chan error, 1)
				go func() { done <- eng.run(ctx) }()

				// run() collects once immediately, then once per tick. Waiting for
				// >=2 collections proves the ticker (not just the initial call)
				// fired.
				require.Eventually(t, func() bool {
					return getMetricsCalls.Load() >= 2
				}, 2*time.Second, 5*time.Millisecond, "ticker should drive more than the initial collection")

				cancel()
				select {
				case err := <-done:
					assert.NoError(t, err, "run() should return nil when the context is cancelled")
				case <-time.After(2 * time.Second):
					t.Fatal("run() did not return after context cancellation")
				}
			},
		},
		{
			name: "context cancellation stops the loop",
			testFunc: func(t *testing.T, eng *Engine, mockClient *mock_dcgm.MockClient, getMetricsCalls *atomic.Int64) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				done := make(chan error, 1)
				go func() { done <- eng.run(ctx) }()

				// Confirm the loop is running before cancelling.
				require.Eventually(t, func() bool {
					return getMetricsCalls.Load() >= 1
				}, 2*time.Second, 5*time.Millisecond, "loop should collect at least once before cancellation")

				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(2 * time.Second):
					t.Fatal("run() did not return after context cancellation")
				}

				// No collection should happen once the loop has returned.
				countAfterStop := getMetricsCalls.Load()
				time.Sleep(50 * time.Millisecond)
				assert.Equal(t, countAfterStop, getMetricsCalls.Load(),
					"no further collections should happen after the loop stops")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			outputPath := filepath.Join(t.TempDir(), "gpu-metrics.json")

			var getMetricsCalls atomic.Int64
			mockClient := mock_dcgm.NewMockClient(ctrl)
			mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
			mockClient.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(
				func(context.Context) ([]gputypes.GPUMetric, error) {
					getMetricsCalls.Add(1)
					return []gputypes.GPUMetric{{GPUUUID: "GPU-tick-1"}}, nil
				}).AnyTimes()
			expectStatus(mockClient, true, "", false)

			// Short interval so the ticker fires many times within the test window.
			eng := newTestEngine(mockClient, outputPath, 5*time.Millisecond)

			tc.testFunc(t, eng, mockClient, &getMetricsCalls)
		})
	}
}

// TestStartCancelsRunLoopOnSIGTERM verifies the shutdown mechanism the systemd
// unit relies on (there is no "stop" command / ExecStop): SIGTERM cancels the
// run loop and Start() returns nil after shutting the client down exactly once.
// It also exercises the Start()-only paths — MkdirAll, ensureFile for the
// metrics and temp files, and the deferred Shutdown. It is not parallel because
// it delivers a signal to the whole test process.
func TestStartCancelsRunLoopOnSIGTERM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Nested path so Start() also has to create a missing parent directory.
	outputPath := filepath.Join(t.TempDir(), "nested", "gpu-metrics.json")

	mockClient := mock_dcgm.NewMockClient(ctrl)
	mockClient.EXPECT().Reconcile(gomock.Any()).Return(true, nil).AnyTimes()
	mockClient.EXPECT().GetMetrics(gomock.Any()).Return([]gputypes.GPUMetric{
		{GPUUUID: "GPU-start-001"},
	}, nil).AnyTimes()
	expectStatus(mockClient, true, "", false)
	mockClient.EXPECT().Shutdown().Return(nil).Times(1)

	// Long interval so the loop blocks in select until the signal fires.
	eng := newTestEngine(mockClient, outputPath, time.Hour)

	done := make(chan error, 1)
	go func() { done <- eng.Start() }()

	// Wait until the run loop is active (the file has been written once), then
	// deliver SIGTERM: NotifyContext should cancel the context and Start() should
	// return nil.
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

	// The metrics file survives; the staging temp file was renamed away.
	_, err := os.Stat(outputPath)
	assert.NoError(t, err, "the metrics file should exist after Start() returns")
}

// TestEnsureFile verifies ensureFile creates a missing file and is a no-op on an
// existing one (it must not truncate), which is what makes it safe to call on
// every restart.
func TestEnsureFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gpu-metrics.json")

	// Missing file: created without error.
	require.NoError(t, ensureFile(path))
	_, err := os.Stat(path)
	require.NoError(t, err, "ensureFile should create the file when it is absent")

	// Existing file with content: ensureFile must neither error nor truncate.
	content := []byte(`{"gpus":[]}`)
	require.NoError(t, os.WriteFile(path, content, metricsFilePermission))

	require.NoError(t, ensureFile(path), "ensureFile should not error when the file already exists")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, got, "ensureFile must not truncate an existing file")
}
