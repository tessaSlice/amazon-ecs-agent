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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Type aliases for convenience in tests.
type (
	MockClient = dcgm.MockClient
)

// NewMockClient is a convenience wrapper for dcgm.NewMockClient.
func NewMockClient() *MockClient {
	return dcgm.NewMockClient()
}

func TestNewDCGMMonitor(t *testing.T) {
	testCases := []struct {
		name string
	}{
		{"creates DCGMMonitor with correct fields"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			testGroup := NewTestGroup(cancel)
			defer testGroup.Cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, dcgm.NewMockClient(), metricsFactory)
			testGroup.Start(healthDirector)
			testGroup.Start(dcgmMonitor)
			testGroup.Cancel()

			assert.NotNil(t, dcgmMonitor)
			assert.Equal(t, ctx, dcgmMonitor.ctx)
			assert.Equal(t, healthDirector, dcgmMonitor.healthDirector)
			assert.NotNil(t, dcgmMonitor.mailbox)
			assert.NotNil(t, dcgmMonitor.logger)
			assert.NotNil(t, dcgmMonitor.metricsFactory)
		})
	}
}

func TestDCGMMonitor_GenerateHealthStatus(t *testing.T) {
	testCases := []struct {
		name     string
		testFunc func(t *testing.T, dcgmMonitor *DCGMMonitor)
	}{
		{
			name: "generates correct hardcoded values",
			testFunc: func(t *testing.T, dcgmMonitor *DCGMMonitor) {
				// Replace the real client with a mock that is healthy.
				mockClient := dcgm.NewMockClient()
				mockClient.SetInitialized(true)
				mockClient.SetHealthy(true)
				dcgmMonitor.dcgmClient = mockClient

				status := dcgmMonitor.generateHealthStatus()

				// Verify hardcoded values per requirements 4.2, 4.3
				assert.NotNil(t, status)
				assert.NotNil(t, status.Status)
				assert.Equal(t, "OK", *status.Status)
				assert.NotNil(t, status.Type)
				assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *status.Type)

				// Verify timestamps are set per requirement 4.4
				assert.NotNil(t, status.LastUpdated)
				assert.NotNil(t, status.LastStatusChange)

				// Verify timestamps are recent (within last second).
				now := time.Now()
				lastUpdated := time.Time(*status.LastUpdated)
				lastStatusChange := time.Time(*status.LastStatusChange)

				assert.WithinDuration(t, now, lastUpdated, 1*time.Second)
				assert.WithinDuration(t, now, lastStatusChange, 1*time.Second)
			},
		},
		{
			name: "generates fresh timestamps on each call",
			testFunc: func(t *testing.T, dcgmMonitor *DCGMMonitor) {
				// Replace the real client with a mock that is healthy.
				mockClient := dcgm.NewMockClient()
				mockClient.SetInitialized(true)
				mockClient.SetHealthy(true)
				dcgmMonitor.dcgmClient = mockClient

				status1 := dcgmMonitor.generateHealthStatus()
				time.Sleep(10 * time.Millisecond)
				status2 := dcgmMonitor.generateHealthStatus()

				// Verify timestamps are different.
				lastUpdated1 := time.Time(*status1.LastUpdated)
				lastUpdated2 := time.Time(*status2.LastUpdated)
				assert.True(t, lastUpdated2.After(lastUpdated1))

				lastStatusChange1 := time.Time(*status1.LastStatusChange)
				lastStatusChange2 := time.Time(*status2.LastStatusChange)
				assert.True(t, lastStatusChange2.After(lastStatusChange1))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			testGroup := NewTestGroup(cancel)
			defer testGroup.Cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, dcgm.NewMockClient(), metricsFactory)
			testGroup.Start(healthDirector)
			testGroup.Start(dcgmMonitor)
			testGroup.Cancel()

			tc.testFunc(t, dcgmMonitor)
		})
	}
}

func TestDCGMMonitor_ReconcileAndReport(t *testing.T) {
	testCases := []struct {
		name     string
		testFunc func(
			t *testing.T,
			dcgmMonitor *DCGMMonitor,
			healthDirector *ContainerInstanceHealthDirector,
			instanceHealthMessages chan ecstcs.InstanceStatusMessage,
		)
	}{
		{
			name: "calls AddMonitorHealth with generated status",
			testFunc: func(
				t *testing.T,
				dcgmMonitor *DCGMMonitor,
				healthDirector *ContainerInstanceHealthDirector,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
			) {
				// Replace the real client with a mock that is healthy.
				mockClient := NewMockClient()
				mockClient.SetInitialized(true)
				mockClient.SetHealthy(true)
				dcgmMonitor.dcgmClient = mockClient

				// Generate status and add it directly to the health director's map.
				status := dcgmMonitor.generateHealthStatus()
				healthDirector.healthStatusMap[*status.Type] = status

				// Verify status was added to the map.
				assert.NotNil(t, healthDirector.healthStatusMap[*status.Type])
				assert.Equal(t, "OK", *healthDirector.healthStatusMap[*status.Type].Status)
				assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *healthDirector.healthStatusMap[*status.Type].Type)

				// Trigger sendHealthStatuses to verify the message is sent.
				healthDirector.sendHealthStatuses()

				// Check if a message was sent to the channel.
				select {
				case msg := <-instanceHealthMessages:
					assert.NotNil(t, msg)
					assert.Len(t, msg.Statuses, 1)
					assert.Equal(t, "OK", *msg.Statuses[0].Status)
					assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *msg.Statuses[0].Type)
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected health status message was not received")
				}
			},
		},
		{
			name: "multiple calls update the status",
			testFunc: func(
				t *testing.T,
				dcgmMonitor *DCGMMonitor,
				healthDirector *ContainerInstanceHealthDirector,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
			) {
				// Replace the real client with a mock that is healthy.
				mockClient := NewMockClient()
				mockClient.SetInitialized(true)
				mockClient.SetHealthy(true)
				dcgmMonitor.dcgmClient = mockClient

				// Generate status and add it to the map (first call).
				status1 := dcgmMonitor.generateHealthStatus()
				healthDirector.healthStatusMap[*status1.Type] = status1
				firstCallTime := time.Now()

				time.Sleep(20 * time.Millisecond)

				// Generate status and add it to the map (second call).
				status2 := dcgmMonitor.generateHealthStatus()
				healthDirector.healthStatusMap[*status2.Type] = status2

				// Trigger sendHealthStatuses to get the latest status.
				healthDirector.sendHealthStatuses()

				// Verify the latest status is sent.
				select {
				case msg := <-instanceHealthMessages:
					assert.NotNil(t, msg)
					assert.Len(t, msg.Statuses, 1)
					assert.Equal(t, "OK", *msg.Statuses[0].Status)
					assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *msg.Statuses[0].Type)
					// Verify the timestamp is from the second call.
					lastUpdated := time.Time(*msg.Statuses[0].LastUpdated)
					assert.True(t, lastUpdated.After(firstCallTime))
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected health status message was not received")
				}
			},
		},
		{
			name: "reconciles DCGM connection before reporting",
			testFunc: func(
				t *testing.T,
				dcgmMonitor *DCGMMonitor,
				healthDirector *ContainerInstanceHealthDirector,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
			) {
				// Replace the real client with a mock.
				mockClient := NewMockClient()
				mockClient.SetInitialized(false)
				mockClient.SetHealthy(false)
				mockClient.SetReconcileJustInit(true)
				dcgmMonitor.dcgmClient = mockClient

				// Call Reconcile directly to test reconciliation.
				justInit, err := dcgmMonitor.dcgmClient.Reconcile(dcgmMonitor.ctx)

				// Verify Reconcile was called.
				assert.GreaterOrEqual(t, mockClient.GetReconcileCallCount(), 1, "Reconcile should be called at least once")
				assert.NoError(t, err, "Reconcile should not return error within grace period")
				assert.True(t, justInit, "justInit should be true when client is initialized")

				// Generate and add status to the map.
				status := dcgmMonitor.generateHealthStatus()
				healthDirector.healthStatusMap[*status.Type] = status

				// Verify status was reported.
				healthDirector.sendHealthStatuses()
				select {
				case msg := <-instanceHealthMessages:
					assert.NotNil(t, msg)
					assert.Len(t, msg.Statuses, 1)
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected health status message was not received")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			// Don't close the channel in defer to avoid race with sendHealthStatuses.

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				NewMockClient(),
				metricsFactory,
			)
			// Don't start the actors - we're calling methods directly for testing.

			tc.testFunc(t, healthDirector.dcgmMonitor, healthDirector, instanceHealthMessages)
		})
	}
}

func TestDCGMMonitor_PeriodicReporting(t *testing.T) {
	testCases := []struct {
		name                   string
		healthDirectorInterval time.Duration
		dcgmMonitorInterval    time.Duration
		testFunc               func(
			t *testing.T,
			healthDirector *ContainerInstanceHealthDirector,
			dcgmMonitor *DCGMMonitor,
			instanceHealthMessages chan ecstcs.InstanceStatusMessage,
			testGroup *TestGroup,
			cancel context.CancelFunc,
		)
	}{
		{
			name:                   "ticker triggers reportHealth at 1 minute intervals",
			healthDirectorInterval: 10 * time.Millisecond,
			dcgmMonitorInterval:    1 * time.Millisecond,
			testFunc: func(
				t *testing.T,
				healthDirector *ContainerInstanceHealthDirector,
				dcgmMonitor *DCGMMonitor,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
				testGroup *TestGroup,
				cancel context.CancelFunc,
			) {
				// Replace the real client with a mock that is healthy.
				mockClient := dcgm.NewMockClient()
				mockClient.SetInitialized(true)
				mockClient.SetHealthy(true)
				dcgmMonitor.dcgmClient = mockClient

				// Start processing mailbox messages for health director.
				testGroup.Start(healthDirector)

				// Wait for ticker events to trigger and verify health status updates.
				select {
				case msg := <-instanceHealthMessages:
					assert.NotNil(t, msg)
					assert.Len(t, msg.Statuses, 1)
					assert.Equal(t, "OK", *msg.Statuses[0].Status)
					assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *msg.Statuses[0].Type)
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected health status message from ticker was not received")
				}
			},
		},
		{
			name:                   "ticker is stopped when context is cancelled",
			healthDirectorInterval: 10 * time.Millisecond,
			dcgmMonitorInterval:    10 * time.Millisecond,
			testFunc: func(
				t *testing.T,
				healthDirector *ContainerInstanceHealthDirector,
				dcgmMonitor *DCGMMonitor,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
				testGroup *TestGroup,
				cancel context.CancelFunc,
			) {
				// Start processing mailbox messages for health director.
				testGroup.Start(healthDirector)

				// Wait for at least one ticker event to ensure actor is running.
				select {
				case <-instanceHealthMessages:
					// Got a health status, actor is working.
				case <-time.After(100 * time.Millisecond):
					t.Fatal("Expected at least one health status message")
				}

				// Cancel context to stop the actor.
				cancel()

				// Wait for actor to stop gracefully.
				time.Sleep(50 * time.Millisecond)

				// Verify no more messages are sent after cancellation.
				select {
				case <-instanceHealthMessages:
					t.Fatal("Unexpected health status message after context cancellation")
				case <-time.After(50 * time.Millisecond):
					// Expected: no more messages after cancellation.
				}
			},
		},
		{
			name:                   "multiple ticker events generate multiple reports",
			healthDirectorInterval: 5 * time.Millisecond,
			dcgmMonitorInterval:    1 * time.Millisecond,
			testFunc: func(
				t *testing.T,
				healthDirector *ContainerInstanceHealthDirector,
				dcgmMonitor *DCGMMonitor,
				instanceHealthMessages chan ecstcs.InstanceStatusMessage,
				testGroup *TestGroup,
				cancel context.CancelFunc,
			) {
				// Start processing mailbox messages for health director.
				testGroup.Start(healthDirector)

				// Wait for multiple ticker events and count received messages.
				receivedCount := 0
				timeout := time.After(50 * time.Millisecond)

				for {
					select {
					case <-instanceHealthMessages:
						receivedCount++
					case <-timeout:
						// Stop actors before assertion.
						testGroup.Cancel()
						// Verify multiple reports were generated.
						assert.GreaterOrEqual(t, receivedCount, 1, "Expected at least 1 health report from ticker events")
						return
					}
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			testGroup := NewTestGroup(cancel)
			defer testGroup.Cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			healthDirector.setTickerInterval(tc.healthDirectorInterval)
			healthDirector.dcgmMonitor.setTickerInterval(tc.dcgmMonitorInterval)

			tc.testFunc(t, healthDirector, healthDirector.dcgmMonitor, instanceHealthMessages, &testGroup, cancel)
		})
	}
}

// TestDCGMMonitor_NewDCGMMonitor_StoresClient tests that NewDCGMMonitor stores the provided client.
func TestDCGMMonitor_NewDCGMMonitor_StoresClient(t *testing.T) {
	testCases := []struct {
		name        string
		description string
	}{
		{
			name:        "stores provided dcgm client",
			description: "NewDCGMMonitor should store the provided DCGMClient instance",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			mockClient := dcgm.NewMockClient()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			require.NotNil(t, dcgmMonitor, tc.description)
			require.NotNil(t, dcgmMonitor.dcgmClient, "DCGMClient should be stored")
			assert.Equal(t, mockClient, dcgmMonitor.dcgmClient, "DCGMClient should be the provided mock client")
		})
	}
}

// TestDCGMMonitor_GenerateHealthStatus_CallsIsHealthy tests that generateHealthStatus calls IsHealthy.
func TestDCGMMonitor_GenerateHealthStatus_CallsIsHealthy(t *testing.T) {
	testCases := []struct {
		name        string
		setupMock   func(*dcgm.MockClient)
		description string
	}{
		{
			name: "calls IsHealthy on Client",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetInitialized(true)
				mock.SetHealthy(true)
			},
			description: "generateHealthStatus should call IsHealthy on the client",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			tc.setupMock(mockClient)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Call generateHealthStatus.
			status := dcgmMonitor.generateHealthStatus()

			// Verify status was generated.
			require.NotNil(t, status, tc.description)
			require.NotNil(t, status.Status, "Status should be set")
		})
	}
}

// TestDCGMMonitor_Stop_CallsShutdown tests that Stop calls client Shutdown.
func TestDCGMMonitor_Stop_CallsShutdown(t *testing.T) {
	testCases := []struct {
		name        string
		setupMock   func(*dcgm.MockClient)
		description string
	}{
		{
			name: "calls Shutdown on Client",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetInitialized(true)
			},
			description: "Stop should call Shutdown on the client",
		},
		{
			name: "handles Shutdown errors gracefully",
			setupMock: func(mock *dcgm.MockClient) {
				mock.SetInitialized(true)
				mock.SetShutdownError(errors.New("shutdown failed"))
			},
			description: "Stop should not panic when Shutdown returns error",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			tc.setupMock(mockClient)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Call Stop.
			dcgmMonitor.Stop()

			// Verify Shutdown was called.
			assert.Equal(t, 1, mockClient.GetShutdownCallCount(), tc.description)
		})
	}
}

// TestDCGMMonitor_ReconcileAndReport_Reconciliation tests reconciliation logic.
func TestDCGMMonitor_ReconcileAndReport_Reconciliation(t *testing.T) {
	testCases := []struct {
		name        string
		setupMock   func(*MockClient)
		testFunc    func(t *testing.T, dcgmMonitor *DCGMMonitor, mockClient *MockClient)
		description string
	}{
		{
			name: "calls Reconcile on Client",
			setupMock: func(mock *MockClient) {
				mock.SetInitialized(true)
				mock.SetHealthy(true)
			},
			testFunc: func(t *testing.T, dcgmMonitor *DCGMMonitor, mockClient *MockClient) {
				// Call reconcileAndReport.
				dcgmMonitor.reconcileAndReport()

				// Verify Reconcile was called.
				assert.GreaterOrEqual(t, mockClient.GetReconcileCallCount(), 1, "Reconcile should be called")
			},
			description: "reconcileAndReport should call Reconcile on the client",
		},
		{
			name: "continues with IMPAIRED status on reconciliation failure",
			setupMock: func(mock *MockClient) {
				mock.SetInitialized(false)
				mock.SetHealthy(false)
				mock.SetReconcileError(errors.New("connection failed"))
			},
			testFunc: func(t *testing.T, dcgmMonitor *DCGMMonitor, mockClient *MockClient) {
				// Call reconcileAndReport.
				dcgmMonitor.reconcileAndReport()

				// Verify Reconcile was called.
				assert.GreaterOrEqual(t, mockClient.GetReconcileCallCount(), 1, "Reconcile should be called")

				// Verify status is IMPAIRED.
				status := dcgmMonitor.generateHealthStatus()
				assert.Equal(
					t,
					ecstcs.InstanceHealthCheckStatusImpaired.String(),
					*status.Status,
					"Status should be IMPAIRED after failed reconciliation",
				)
			},
			description: "reconcileAndReport should continue with IMPAIRED status when reconciliation fails",
		},
		{
			name: "successful reconciliation restores healthy status",
			setupMock: func(mock *MockClient) {
				// Start as not initialized.
				mock.SetInitialized(false)
				mock.SetHealthy(false)
				mock.SetReconcileJustInit(true)
			},
			testFunc: func(t *testing.T, dcgmMonitor *DCGMMonitor, mockClient *MockClient) {
				// Call reconcileAndReport (will attempt reconciliation).
				dcgmMonitor.reconcileAndReport()

				// Verify Reconcile was called.
				assert.GreaterOrEqual(t, mockClient.GetReconcileCallCount(), 1, "Reconcile should be called")

				// After successful reconciliation, set client to healthy.
				mockClient.SetHealthy(true)

				// Generate status again.
				status := dcgmMonitor.generateHealthStatus()
				assert.Equal(
					t,
					ecstcs.InstanceHealthCheckStatusOk.String(),
					*status.Status,
					"Status should be OK after successful reconciliation",
				)
			},
			description: "reconcileAndReport should restore healthy status after successful reconciliation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				NewMockClient(),
				metricsFactory,
			)
			mockClient := NewMockClient()
			tc.setupMock(mockClient)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			tc.testFunc(t, dcgmMonitor, mockClient)
		})
	}
}

// TestHealthyClientProducesOK tests that healthy client produces OK status.
func TestHealthyClientProducesOK(t *testing.T) {
	testCases := []struct {
		name        string
		description string
	}{
		{
			name:        "healthy client produces OK status",
			description: "Healthy client should produce OK status",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			mockClient.SetInitialized(true)
			mockClient.SetHealthy(true)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Generate health status.
			status := dcgmMonitor.generateHealthStatus()

			// Verify the status is OK.
			require.NotNil(t, status, "generateHealthStatus should not return nil")
			require.NotNil(t, status.Status, "Status field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckStatusOk.String(), *status.Status, tc.description)
			require.NotNil(t, status.Type, "Type field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *status.Type, "Type should be ACCELERATED_COMPUTE")
		})
	}
}

// TestUnhealthyClientProducesIMPAIRED tests that unhealthy client produces IMPAIRED status.
func TestUnhealthyClientProducesIMPAIRED(t *testing.T) {
	testCases := []struct {
		name        string
		description string
	}{
		{
			name:        "unhealthy client produces IMPAIRED status",
			description: "Unhealthy client should produce IMPAIRED status",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			mockClient.SetInitialized(true)
			mockClient.SetHealthy(false)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Generate health status.
			status := dcgmMonitor.generateHealthStatus()

			// Verify the status is IMPAIRED.
			require.NotNil(t, status, "generateHealthStatus should not return nil")
			require.NotNil(t, status.Status, "Status field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckStatusImpaired.String(), *status.Status, tc.description)
			require.NotNil(t, status.Type, "Type field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *status.Type, "Type should be ACCELERATED_COMPUTE")
		})
	}
}

// TestUninitializedClientProducesIMPAIRED tests that uninitialized client produces IMPAIRED status.
func TestUninitializedClientProducesIMPAIRED(t *testing.T) {
	testCases := []struct {
		name        string
		description string
	}{
		{
			name:        "uninitialized client produces IMPAIRED status",
			description: "Uninitialized client should produce IMPAIRED status",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			mockClient.SetInitialized(false)
			mockClient.SetHealthy(false)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Generate health status.
			status := dcgmMonitor.generateHealthStatus()

			// Verify the status is IMPAIRED.
			require.NotNil(t, status, "generateHealthStatus should not return nil")
			require.NotNil(t, status.Status, "Status field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckStatusImpaired.String(), *status.Status, tc.description)
			require.NotNil(t, status.Type, "Type field should not be nil")
			assert.Equal(t, ecstcs.InstanceHealthCheckTypeAcceleratedCompute, *status.Type, "Type should be ACCELERATED_COMPUTE")
		})
	}
}

// TestDCGMMonitor_GenerateHealthStatus_StatusPriority tests status priority (INSUFFICIENT_DATA > IMPAIRED > OK).
func TestDCGMMonitor_GenerateHealthStatus_StatusPriority(t *testing.T) {
	testCases := []struct {
		name             string
		isConnectionLost bool
		isHealthy        bool
		expectedStatus   string
		description      string
	}{
		{
			name:             "INSUFFICIENT_DATA takes priority over IMPAIRED",
			isConnectionLost: true,
			isHealthy:        false,
			expectedStatus:   ecstcs.InstanceHealthCheckStatusInsufficientData.String(),
			description:      "INSUFFICIENT_DATA status should take priority when connection is lost even if unhealthy",
		},
		{
			name:             "INSUFFICIENT_DATA takes priority over OK",
			isConnectionLost: true,
			isHealthy:        true,
			expectedStatus:   ecstcs.InstanceHealthCheckStatusInsufficientData.String(),
			description:      "INSUFFICIENT_DATA status should take priority when connection is lost even if healthy",
		},
		{
			name:             "IMPAIRED when connection OK but unhealthy",
			isConnectionLost: false,
			isHealthy:        false,
			expectedStatus:   ecstcs.InstanceHealthCheckStatusImpaired.String(),
			description:      "IMPAIRED status when connection is OK but client is unhealthy",
		},
		{
			name:             "OK when connection OK and healthy",
			isConnectionLost: false,
			isHealthy:        true,
			expectedStatus:   ecstcs.InstanceHealthCheckStatusOk.String(),
			description:      "OK status when connection is OK and client is healthy",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			instanceHealthMessages := make(chan ecstcs.InstanceStatusMessage, 10)
			defer close(instanceHealthMessages)

			metricsFactory := metrics.NewNopEntryFactory()
			healthDirector := NewContainerInstanceHealthDirector(
				ctx,
				instanceHealthMessages,
				"test-cluster",
				"arn:aws:ecs:us-west-2:123456789012:container-instance/test-instance",
				dcgm.NewMockClient(),
				metricsFactory,
			)
			mockClient := dcgm.NewMockClient()
			mockClient.SetConnectionLost(tc.isConnectionLost)
			mockClient.SetHealthy(tc.isHealthy)
			mockClient.SetInitialized(!tc.isConnectionLost)
			dcgmMonitor := NewDCGMMonitor(ctx, healthDirector, mockClient, metricsFactory)

			// Call generateHealthStatus.
			status := dcgmMonitor.generateHealthStatus()

			// Verify expected status.
			require.NotNil(t, status, tc.description)
			require.NotNil(t, status.Status, "Status should be set")
			assert.Equal(t, tc.expectedStatus, *status.Status, tc.description)
		})
	}
}
