//go:build linux

package gpu

import (
	"context"
	"time"

	"github.com/aws/two/agent/dcgm"
	"github.com/aws/two/agent/metrics"

	"go.uber.org/zap"
)

const (
	// defaultGPUMetricsCollectionInterval is the interval at which GPU metrics
	// are collected from DCGM and forwarded to the MetricsDirector.
	defaultGPUMetricsCollectionInterval = 60 * time.Second
)

// GPUMetricsCollector periodically collects GPU telemetry from DCGM and
// forwards the metrics to the MetricsDirector for TACS publishing.
type GPUMetricsCollector struct {
	basicActor

	// dcgmClient is the shared DCGM client for GPU metrics collection.
	dcgmClient dcgm.Client

	// metricsDirector receives GPU metrics for inclusion in TelemetryMessages.
	metricsDirector *MetricsDirector

	// tickerInterval is the collection interval (default 60s, configurable for testing).
	tickerInterval time.Duration
}

// NewGPUMetricsCollector creates a new GPUMetricsCollector instance with proper
// basicActor initialization and logging setup.
func NewGPUMetricsCollector(
	ctx context.Context,
	dcgmClient dcgm.Client,
	metricsDirector *MetricsDirector,
	metricsFactory metrics.EntryFactory,
) *GPUMetricsCollector {
	logger := zap.L().With(zap.String("Actor", "GPUMetricsCollector"))

	return &GPUMetricsCollector{
		basicActor: basicActor{
			ctx:     ctx,
			mailbox: make(chan func(), actorMailboxSizeDefault),
			logger: func() *zap.Logger {
				return logger
			},
			metricsFactory: metricsFactory,
		},
		dcgmClient:      dcgmClient,
		metricsDirector: metricsDirector,
		tickerInterval:  defaultGPUMetricsCollectionInterval,
	}
}

// setTickerInterval sets the ticker interval for testing purposes.
// This method should only be called before Start() is invoked.
func (c *GPUMetricsCollector) setTickerInterval(interval time.Duration) {
	c.tickerInterval = interval
}

// Start implements the actor Start method with standard actor message loop and
// configurable ticker. It creates a ticker that triggers at the configured
// interval to reconcile the DCGM connection and collect GPU metrics.
func (c *GPUMetricsCollector) Start() {
	c.logger().Info("Starting GPU metrics collector.")

	ticker := time.NewTicker(c.tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case f := <-c.mailbox:
			f()
		case <-ticker.C:
			c.reconcileAndCollect()
		case <-c.ctx.Done():
			c.logger().Info("Stopped GPU metrics collector.")
			return
		}
	}
}

// reconcileAndCollect performs DCGM connection reconciliation and collects GPU
// metrics. It first attempts to reconcile the DCGM connection, then collects
// metrics and forwards them to the MetricsDirector.
func (c *GPUMetricsCollector) reconcileAndCollect() {
	// Reconcile DCGM connection before collecting metrics.
	if _, err := c.dcgmClient.Reconcile(c.ctx); err != nil {
		c.logger().Warn("Failed to reconcile DCGM client, skipping metrics collection.", zap.Error(err))
		return
	}

	// Collect GPU metrics from all devices.
	gpuMetrics, err := c.dcgmClient.GetMetrics(c.ctx)
	if err != nil {
		c.logger().Warn("Failed to get GPU metrics, skipping forwarding.", zap.Error(err))
		return
	}

	c.logger().Debug("Collected GPU metrics.", zap.Int("deviceCount", len(gpuMetrics)))

	// Forward metrics to MetricsDirector.
	c.metricsDirector.SetGPUMetrics(gpuMetrics)
}
