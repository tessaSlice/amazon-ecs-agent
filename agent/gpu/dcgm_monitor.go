//go:build linux

package gpu

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/two/agent/dcgm"
	"github.com/aws/two/agent/metrics"

	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils"
	"github.com/aws/aws-sdk-go/aws"
	"go.uber.org/zap"
)

// DCGMMonitor monitors NVIDIA accelerated hardware health status and reports it
// to the ContainerInstanceHealthDirector. It follows the actor pattern by extending
// basicActor and implementing periodic health status generation.
type DCGMMonitor struct {
	basicActor

	// Reference to ContainerInstanceHealthDirector for status reporting.
	healthDirector *ContainerInstanceHealthDirector

	// DCGM client for Nvidia GPU health monitoring.
	dcgmClient dcgm.Client

	// Ticker for periodic health status generation.
	ticker *time.Ticker

	// Ticker interval for health status generation (configurable for testing).
	tickerInterval time.Duration

	// Metrics factory for emitting CloudWatch metrics.
	metricsFactory metrics.EntryFactory
}

// NewDCGMMonitor creates a new DCGMMonitor instance with proper basicActor
// initialization and logging setup. The dcgmClient parameter is a shared DCGM
// client instance created by the ContainerInstanceDirector.
func NewDCGMMonitor(
	ctx context.Context,
	healthDirector *ContainerInstanceHealthDirector,
	dcgmClient dcgm.Client,
	metricsFactory metrics.EntryFactory,
) *DCGMMonitor {
	logger := zap.L().With(zap.String("Actor", "DCGMMonitor"))

	return &DCGMMonitor{
		basicActor: basicActor{
			ctx:     ctx,
			mailbox: make(chan func(), 50),
			logger: func() *zap.Logger {
				return logger
			},
		},
		healthDirector: healthDirector,
		dcgmClient:     dcgmClient,
		tickerInterval: 1 * time.Minute,
		metricsFactory: metricsFactory,
	}
}

// setTickerInterval sets the ticker interval for testing purposes.
// This method should only be called before Start() is invoked.
func (dm *DCGMMonitor) setTickerInterval(interval time.Duration) {
	dm.tickerInterval = interval
}

// Start implements the actor Start method with standard actor message loop and configurable ticker.
// It creates a ticker that triggers at the configured interval to reconcile DCGM connection and report health status.
func (dm *DCGMMonitor) Start() {
	dm.logger().Info("starting")

	// Perform initial reconciliation to establish DCGM connection.
	if justInit, err := dm.dcgmClient.Reconcile(dm.ctx); err != nil {
		dm.logger().Error("failed to reconcile DCGM client on startup", zap.Error(err))
		// Continue operation even if reconciliation fails - will retry on next tick.
	} else if justInit {
		dm.logger().Info("DCGM client initialized successfully on startup")
	}

	// Report initial health status immediately after reconciliation.
	initialStatus := dm.generateHealthStatus()
	dm.healthDirector.AddMonitorHealth(initialStatus)
	dm.logger().Info("reported initial health status to director",
		zap.String("status", aws.StringValue(initialStatus.Status)),
		zap.String("type", aws.StringValue(initialStatus.Type)),
	)

	dm.ticker = time.NewTicker(dm.tickerInterval)
	defer dm.ticker.Stop()

	for {
		select {
		case f := <-dm.mailbox:
			f()
		case <-dm.ticker.C:
			dm.reconcileAndReport()
		case <-dm.ctx.Done():
			dm.logger().Debug("stopped")
			return
		}
	}
}

// reconcileAndReport performs DCGM connection reconciliation and reports health status.
// It first attempts to reconcile the DCGM connection (health check and reconnect if needed),
// then generates and reports the current health status to the ContainerInstanceHealthDirector.
func (dm *DCGMMonitor) reconcileAndReport() {
	dm.logger().Info("starting reconciliation and health status report")

	// Attempt to reconcile DCGM connection.
	justInit, err := dm.dcgmClient.Reconcile(dm.ctx)
	switch {
	case err != nil:
		dm.logger().Error("failed to reconcile DCGM client",
			zap.Error(err),
			zap.String("status", ecstcs.InstanceHealthCheckStatusImpaired.String()))
		metrics.Capture(dm.metricsFactory, metrics.GPUDCGMConnectionFailureMetricName).Done(err)
		// Continue to report status even if reconciliation fails.
	case justInit:
		dm.logger().Info("DCGM client reinitialized successfully")
	default:
		dm.logger().Debug("DCGM client connection is healthy")
	}

	// Generate and report health status.
	status := dm.generateHealthStatus()
	dm.healthDirector.AddMonitorHealth(status)
	dm.logger().Info("reported health status to director",
		zap.String("status", aws.StringValue(status.Status)),
		zap.String("type", aws.StringValue(status.Type)),
	)
}

// generateHealthStatus creates an ecstcs.InstanceStatus from the DCGMClient.
func (dm *DCGMMonitor) generateHealthStatus() *ecstcs.InstanceStatus {
	now := new(time.Now())
	monitorType := ecstcs.InstanceHealthCheckTypeAcceleratedCompute

	var status string
	var statusReason string

	switch {
	case dm.dcgmClient.IsConnectionLost():
		status = ecstcs.InstanceHealthCheckStatusInsufficientData.String()
		// No StatusReason for connection lost.
		metrics.Capture(dm.metricsFactory, metrics.GPUInsufficientDataMetricName).Done(
			fmt.Errorf("instance health status is INSUFFICIENT_DATA"))
	case dm.dcgmClient.IsHealthy():
		status = ecstcs.InstanceHealthCheckStatusOk.String()
		// No StatusReason for healthy.
	default:
		status = ecstcs.InstanceHealthCheckStatusImpaired.String()
		statusReason = dm.dcgmClient.UnhealthyReason()
		metrics.Capture(dm.metricsFactory, metrics.GPUImpairedMetricName).Done(
			fmt.Errorf("instance health status is IMPAIRED"))
	}

	dm.logger().Info("generated health status",
		zap.String("status", status),
		zap.Bool("isHealthy", dm.dcgmClient.IsHealthy()),
		zap.Bool("isConnectionLost", dm.dcgmClient.IsConnectionLost()))

	instanceStatus := &ecstcs.InstanceStatus{
		Status:           &status,
		Type:             &monitorType,
		LastStatusChange: (*utils.Timestamp)(now),
		LastUpdated:      (*utils.Timestamp)(now),
	}

	if statusReason != "" {
		instanceStatus.StatusReason = &statusReason
	}

	return instanceStatus
}

// Stop shuts down the DCGMMonitor and cleans up DCGM resources.
// It calls dcgmClient.Shutdown to disconnect from the DCGM host engine.
// Shutdown errors are logged but do not block termination.
func (dm *DCGMMonitor) Stop() {
	dm.logger().Info("stopping, shutting down client")

	if err := dm.dcgmClient.Shutdown(); err != nil {
		dm.logger().Error("failed to shutdown client during stop", zap.Error(err))
	} else {
		dm.logger().Info("client shutdown successfully")
	}
}
