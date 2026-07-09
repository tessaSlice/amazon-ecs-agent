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

// expectStatus stubs the health-reporting methods every reconcileAndCollect
// invokes once it gets past reconciliation. Values are asserted through the
// written file, not here.
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

func TestNewEngine(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
	}{
		{"creates Engine with correct fields"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, err := New()

			require.NoError(t, err)
			require.NotNil(t, eng)
			assert.NotNil(t, eng.client)
			assert.Equal(t, MetricsFilePath, eng.outputPath)
			assert.Equal(t, metricsCollectionInterval, eng.collectionInterval)
			assert.Equal(t, MetricsFilePath+".tmp", eng.tempPath())
		})
	}
}

func TestEngine_ReconcileAndCollect(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		setupMock        func(*mock_dcgm.MockClient, *atomic.Int64, *atomic.Int64)
		expectGetMetrics bool
		expectWrite      bool
		verify           func(t *testing.T, out dcgmOutput)
	}{
		{
			name: "reconcile failure skips GetMetrics",
			setupMock: func(m *mock_dcgm.MockClient, reconciles, getMetrics *atomic.Int64) {
				m.EXPECT().Reconcile(gomock.Any()).DoAndReturn(func(context.Context) (bool, error) {
					reconciles.Add(1)
					return false, errors.New("connection failed")
				}).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(func(context.Context) ([]gputypes.GPUMetric, error) {
					getMetrics.Add(1)
					return nil, nil
				}).AnyTimes()
			},
			expectGetMetrics: false,
			expectWrite:      false,
		},
		{
			name: "GetMetrics failure skips file write of metrics",
			setupMock: func(m *mock_dcgm.MockClient, reconciles, getMetrics *atomic.Int64) {
				m.EXPECT().Reconcile(gomock.Any()).DoAndReturn(func(context.Context) (bool, error) {
					reconciles.Add(1)
					return true, nil
				}).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(func(context.Context) ([]gputypes.GPUMetric, error) {
					getMetrics.Add(1)
					return nil, errors.New("metrics unavailable")
				}).AnyTimes()
				expectStatus(m, true, "", true)
			},
			expectGetMetrics: true,
			// A GetMetrics failure is non-fatal: a status snapshot with no per-GPU
			// metrics is still written (the file-sink analog of the reference's
			// "status only, no SetGPUMetrics").
			expectWrite: true,
			verify: func(t *testing.T, out dcgmOutput) {
				assert.NotEmpty(t, out.Timestamp, "status file should carry a fresh timestamp")
				assert.True(t, out.ConnectionLost, "status file should reflect connection lost")
				assert.Empty(t, out.GPUs, "no per-GPU metrics should be present on collection failure")
			},
		},
		{
			name: "successful collection writes metrics to file",
			setupMock: func(m *mock_dcgm.MockClient, reconciles, getMetrics *atomic.Int64) {
				m.EXPECT().Reconcile(gomock.Any()).DoAndReturn(func(context.Context) (bool, error) {
					reconciles.Add(1)
					return true, nil
				}).AnyTimes()
				utilization := 75.0
				m.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(func(context.Context) ([]gputypes.GPUMetric, error) {
					getMetrics.Add(1)
					return []gputypes.GPUMetric{
						{GPUUUID: "GPU-test-uuid-1", GPUUtilization: &utilization},
					}, nil
				}).AnyTimes()
				expectStatus(m, true, "", false)
			},
			expectGetMetrics: true,
			expectWrite:      true,
			verify: func(t *testing.T, out dcgmOutput) {
				assert.True(t, out.Healthy)
				assert.Empty(t, out.UnhealthyReason)
				assert.False(t, out.ConnectionLost)
				require.Len(t, out.GPUs, 1)
				assert.Equal(t, "GPU-test-uuid-1", out.GPUs[0].GPUUUID)
				require.NotNil(t, out.GPUs[0].GPUUtilization)
				assert.Equal(t, 75.0, *out.GPUs[0].GPUUtilization)
			},
		},
		{
			name: "unhealthy GPU records the XID reason",
			setupMock: func(m *mock_dcgm.MockClient, reconciles, getMetrics *atomic.Int64) {
				m.EXPECT().Reconcile(gomock.Any()).DoAndReturn(func(context.Context) (bool, error) {
					reconciles.Add(1)
					return true, nil
				}).AnyTimes()
				m.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(func(context.Context) ([]gputypes.GPUMetric, error) {
					getMetrics.Add(1)
					return []gputypes.GPUMetric{{GPUUUID: "GPU-unhealthy-001"}}, nil
				}).AnyTimes()
				expectStatus(m, false, "XID_48", false)
			},
			expectGetMetrics: true,
			expectWrite:      true,
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

			var reconcileCalls, getMetricsCalls atomic.Int64
			mockClient := mock_dcgm.NewMockClient(ctrl)
			tc.setupMock(mockClient, &reconcileCalls, &getMetricsCalls)

			eng := newTestEngine(mockClient, outputPath, time.Hour)

			// Call reconcileAndCollect directly (no need to run the loop since we
			// are not exercising the ticker here).
			require.NoError(t, eng.reconcileAndCollect(context.Background()))

			// Verify GetMetrics was called (or not) based on the Reconcile outcome.
			if tc.expectGetMetrics {
				assert.GreaterOrEqual(t, getMetricsCalls.Load(), int64(1),
					"GetMetrics should have been called")
			} else {
				assert.Equal(t, int64(0), getMetricsCalls.Load(),
					"GetMetrics should not have been called")
			}

			// Verify Reconcile was always called.
			assert.GreaterOrEqual(t, reconcileCalls.Load(), int64(1),
				"Reconcile should always be called")

			// Verify the metrics were written to the file (the file-sink analog of
			// the reference draining the MetricsDirector mailbox).
			if tc.expectWrite {
				// The staging temp file must have been renamed away.
				_, err := os.Stat(eng.tempPath())
				assert.True(t, os.IsNotExist(err), "temp file should not remain after the atomic rename")
				if tc.verify != nil {
					tc.verify(t, readOutput(t, outputPath))
				}
			} else {
				// Verify nothing was written.
				_, err := os.Stat(outputPath)
				assert.True(t, os.IsNotExist(err), "no file should be written when reconciliation fails")
			}
		})
	}
}

func TestEngine_PeriodicCollection(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		testFunc func(
			t *testing.T,
			eng *Engine,
			mockClient *mock_dcgm.MockClient,
			reconcileCalls *atomic.Int64,
			getMetricsCalls *atomic.Int64,
			cancel context.CancelFunc,
		)
	}{
		{
			name: "ticker triggers collection",
			testFunc: func(
				t *testing.T,
				eng *Engine,
				mockClient *mock_dcgm.MockClient,
				reconcileCalls *atomic.Int64,
				getMetricsCalls *atomic.Int64,
				cancel context.CancelFunc,
			) {
				// Wait for at least one collection tick.
				assert.Eventually(t, func() bool {
					return getMetricsCalls.Load() >= 1
				}, 200*time.Millisecond, 5*time.Millisecond,
					"Expected at least one GetMetrics call from ticker")
			},
		},
		{
			name: "context cancellation stops the loop",
			testFunc: func(
				t *testing.T,
				eng *Engine,
				mockClient *mock_dcgm.MockClient,
				reconcileCalls *atomic.Int64,
				getMetricsCalls *atomic.Int64,
				cancel context.CancelFunc,
			) {
				// Wait for at least one tick to confirm the loop is running.
				assert.Eventually(t, func() bool {
					return reconcileCalls.Load() >= 1
				}, 200*time.Millisecond, 5*time.Millisecond,
					"Expected at least one Reconcile call")

				// Cancel the context to stop the loop.
				cancel()

				// Record the call count after cancellation.
				time.Sleep(50 * time.Millisecond)
				countAfterCancel := reconcileCalls.Load()

				// Verify no more calls happen after cancellation.
				time.Sleep(50 * time.Millisecond)
				assert.Equal(t, countAfterCancel, reconcileCalls.Load(),
					"No more Reconcile calls should happen after context cancellation")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			outputPath := filepath.Join(t.TempDir(), "gpu-metrics.json")

			var reconcileCalls, getMetricsCalls atomic.Int64
			mockClient := mock_dcgm.NewMockClient(ctrl)
			mockClient.EXPECT().Reconcile(gomock.Any()).DoAndReturn(func(context.Context) (bool, error) {
				reconcileCalls.Add(1)
				return true, nil
			}).AnyTimes()
			mockClient.EXPECT().GetMetrics(gomock.Any()).DoAndReturn(func(context.Context) ([]gputypes.GPUMetric, error) {
				getMetricsCalls.Add(1)
				return []gputypes.GPUMetric{{GPUUUID: "GPU-tick-1"}}, nil
			}).AnyTimes()
			expectStatus(mockClient, true, "", false)

			// Short interval so the ticker fires many times within the test window.
			eng := newTestEngine(mockClient, outputPath, 5*time.Millisecond)

			done := make(chan error, 1)
			go func() { done <- eng.run(ctx) }()

			tc.testFunc(t, eng, mockClient, &reconcileCalls, &getMetricsCalls, cancel)

			// Cancel (idempotent) and confirm the loop returns cleanly.
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err, "run() should return nil when the context is cancelled")
			case <-time.After(2 * time.Second):
				t.Fatal("run() did not return after context cancellation")
			}
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
