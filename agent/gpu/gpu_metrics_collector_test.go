//go:build unit && linux

package gpu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/two/agent/dcgm"
	"github.com/aws/two/agent/metrics"

	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGPUMetricsCollector(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
	}{
		{"creates GPUMetricsCollector with correct fields"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			mockClient := dcgm.NewMockClient()
			metricsFactory := metrics.NewNopEntryFactory()
			telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
			healthMessages := make(chan ecstcs.HealthMessage, 10)

			md := NewMetricsDirector(ctx, telemetryMessages, healthMessages,
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test",
				"test-cluster", nil, nil, metricsFactory)

			collector := NewGPUMetricsCollector(ctx, mockClient, md, metricsFactory)

			require.NotNil(t, collector)
			assert.Equal(t, ctx, collector.ctx)
			assert.NotNil(t, collector.mailbox)
			assert.NotNil(t, collector.logger)
			assert.NotNil(t, collector.dcgmClient)
			assert.Equal(t, md, collector.metricsDirector)
			assert.Equal(t, defaultGPUMetricsCollectionInterval, collector.tickerInterval)
		})
	}
}

func TestGPUMetricsCollector_ReconcileAndCollect(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name               string
		setupMock          func(*dcgm.MockClient)
		expectGetMetrics   bool
		expectSetGPUCalled bool
	}{
		{
			name: "reconcile failure skips GetMetrics",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetReconcileError(errors.New("connection failed"))
			},
			expectGetMetrics:   false,
			expectSetGPUCalled: false,
		},
		{
			name: "GetMetrics failure skips SetGPUMetrics",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetInitialized(true)
				mock.SetMetricsError(errors.New("metrics unavailable"))
			},
			expectGetMetrics:   true,
			expectSetGPUCalled: false,
		},
		{
			name: "successful collection forwards metrics to MetricsDirector",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetInitialized(true)
				mock.SetMetrics([]dcgm.GPUMetric{
					{
						GPUUUID:        "GPU-test-uuid-1",
						GPUUtilization: aws.Float64(75.0),
					},
				})
			},
			expectGetMetrics:   true,
			expectSetGPUCalled: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			mockClient := dcgm.NewMockClient()
			tc.setupMock(mockClient)

			metricsFactory := metrics.NewNopEntryFactory()
			telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
			healthMessages := make(chan ecstcs.HealthMessage, 10)

			md := NewMetricsDirector(ctx, telemetryMessages, healthMessages,
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test",
				"test-cluster", nil, nil, metricsFactory)

			collector := NewGPUMetricsCollector(ctx, mockClient, md, metricsFactory)

			// Call reconcileAndCollect directly (no need for TestGroup since we're not calling Start).
			collector.reconcileAndCollect()

			// Verify GetMetrics was called (or not) based on Reconcile outcome.
			if tc.expectGetMetrics {
				assert.GreaterOrEqual(t, mockClient.GetMetricsCallCount(), 1,
					"GetMetrics should have been called")
			} else {
				assert.Equal(t, 0, mockClient.GetMetricsCallCount(),
					"GetMetrics should not have been called")
			}

			// Verify Reconcile was always called.
			assert.GreaterOrEqual(t, mockClient.GetReconcileCallCount(), 1,
				"Reconcile should always be called")

			// Verify SetGPUMetrics was called by draining the MetricsDirector mailbox.
			if tc.expectSetGPUCalled {
				// Process the mailbox message to verify it was enqueued.
				select {
				case f := <-md.mailbox:
					f()
					assert.NotNil(t, md.latestGPUMetrics,
						"GPU metrics should be stored in MetricsDirector")
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected SetGPUMetrics message in MetricsDirector mailbox")
				}
			} else {
				// Verify no message was enqueued.
				select {
				case <-md.mailbox:
					t.Fatal("Unexpected message in MetricsDirector mailbox")
				case <-time.After(50 * time.Millisecond):
					// Expected: no message.
				}
			}
		})
	}
}

func TestGPUMetricsCollector_PeriodicCollection(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		testFunc func(
			t *testing.T,
			collector *GPUMetricsCollector,
			md *MetricsDirector,
			mockClient *dcgm.MockClient,
			testGroup *TestGroup,
			cancel context.CancelFunc,
		)
	}{
		{
			name: "ticker triggers collection",
			testFunc: func(
				t *testing.T,
				collector *GPUMetricsCollector,
				md *MetricsDirector,
				mockClient *dcgm.MockClient,
				testGroup *TestGroup,
				cancel context.CancelFunc,
			) {
				mockClient.SetInitialized(true)
				mockClient.SetMetrics([]dcgm.GPUMetric{
					{GPUUUID: "GPU-tick-1", GPUUtilization: aws.Float64(50.0)},
				})

				testGroup.Start(md)
				testGroup.Start(collector)

				// Wait for at least one collection tick.
				ok := testGroup.WaitForCondition(func() bool {
					return mockClient.GetMetricsCallCount() >= 1
				}, 200*time.Millisecond, 5*time.Millisecond)
				assert.True(t, ok, "Expected at least one GetMetrics call from ticker")
			},
		},
		{
			name: "context cancellation stops the actor",
			testFunc: func(
				t *testing.T,
				collector *GPUMetricsCollector,
				md *MetricsDirector,
				mockClient *dcgm.MockClient,
				testGroup *TestGroup,
				cancel context.CancelFunc,
			) {
				mockClient.SetInitialized(true)
				mockClient.SetMetrics([]dcgm.GPUMetric{
					{GPUUUID: "GPU-cancel-1"},
				})

				testGroup.Start(md)
				testGroup.Start(collector)

				// Wait for at least one tick to confirm actor is running.
				ok := testGroup.WaitForCondition(func() bool {
					return mockClient.GetReconcileCallCount() >= 1
				}, 200*time.Millisecond, 5*time.Millisecond)
				assert.True(t, ok, "Expected at least one Reconcile call")

				// Cancel context to stop the actor.
				cancel()

				// Record call count after cancellation.
				time.Sleep(50 * time.Millisecond)
				countAfterCancel := mockClient.GetReconcileCallCount()

				// Verify no more calls happen after cancellation.
				time.Sleep(50 * time.Millisecond)
				assert.Equal(t, countAfterCancel, mockClient.GetReconcileCallCount(),
					"No more Reconcile calls should happen after context cancellation")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			testGroup := NewTestGroup(cancel)
			defer testGroup.Cancel()

			mockClient := dcgm.NewMockClient()
			metricsFactory := metrics.NewNopEntryFactory()
			telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
			healthMessages := make(chan ecstcs.HealthMessage, 10)

			md := NewMetricsDirector(ctx, telemetryMessages, healthMessages,
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test",
				"test-cluster", nil, nil, metricsFactory)

			collector := NewGPUMetricsCollector(ctx, mockClient, md, metricsFactory)
			collector.setTickerInterval(5 * time.Millisecond)

			tc.testFunc(t, collector, md, mockClient, &testGroup, cancel)
		})
	}
}
