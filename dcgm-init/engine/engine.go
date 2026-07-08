//go:build linux

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
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/gpu/dcgm"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
)

const (
	// DefaultInitErrorExitCode is the exit code used for general init errors.
	DefaultInitErrorExitCode = -1
)

const (
	MetricsDirectory    = "/var/run/ecs/"
	MetricsFilePath     = "/var/run/ecs/gpu-metrics.json"
	TempMetricsFilePath = "/var/run/ecs/gpu-metrics.json.tmp"

	// metricsDirPermission is the permission for the directory holding the
	// metrics file. 0755 lets the agent (which bind-mounts /var/run/ecs
	// read-only) traverse into the directory to read the metrics file.
	metricsDirPermission os.FileMode = 0755

	// metricsFilePermission is the permission for the metrics file and its
	// staging temp file. 0644 keeps the file world-readable so the agent can
	// consume it; only dcgm-init (running as root) writes it.
	metricsFilePermission os.FileMode = 0644

	// metricsCollectionInterval is how often GPU metrics are collected and
	// written. The agent samples the file at roughly this cadence, so writing
	// more frequently would not surface additional data downstream.
	metricsCollectionInterval = 60 * time.Second
)

// Engine drives the dcgm-init metrics collection loop: it connects to DCGM via
// the dcgm.Client, periodically collects GPU metrics, and writes them to a
// shared JSON file that the agent reads.
type Engine struct {
	client dcgm.Client
}

// New creates an instance of Engine. The DCGM client is created here but does
// not connect until the first Reconcile inside the collection loop, so New is
// cheap and does not fail on hosts where nv-hostengine is not yet up.
func New() (*Engine, error) {
	return &Engine{
		client: dcgm.NewClient(dcgm.Config{}),
	}, nil
}

// Start prepares the output location and then runs the metrics collection loop
// until a SIGTERM/SIGINT is received (systemd's default stop sends SIGTERM).
func (e *Engine) Start() error {
	// Ensure the output directory exists. MkdirAll is a no-op when the
	// directory is already present; any other failure means we have nowhere to
	// write metrics, so fail fast and let systemd surface the error.
	if err := os.MkdirAll(MetricsDirectory, metricsDirPermission); err != nil {
		return fmt.Errorf("failed to create metrics directory %s: %w", MetricsDirectory, err)
	}

	// Ensure the metrics file and its staging temp file exist so the agent,
	// which bind-mounts this directory read-only, always finds a file to read
	// even before the first collection completes (DCGM initialization can take
	// up to the grace period). ensureFile leaves an existing file untouched, so
	// this is safe across restarts; only an inability to create the files is
	// fatal.
	if err := ensureFile(MetricsFilePath); err != nil {
		return fmt.Errorf("failed to create metrics file %s: %w", MetricsFilePath, err)
	}
	if err := ensureFile(TempMetricsFilePath); err != nil {
		return fmt.Errorf("failed to create temp metrics file %s: %w", TempMetricsFilePath, err)
	}

	// Cancel the context when a shutdown signal arrives so the run loop unwinds
	// cleanly. There is no separate "stop" command; shutdown is signal-driven
	// (systemd's default stop sends SIGTERM). NotifyContext installs the signal
	// handler and returns a context that is cancelled on SIGINT/SIGTERM; stop
	// removes the handler when we return.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Release DCGM/nv-hostengine resources on the way out.
	defer func() {
		if err := e.client.Shutdown(); err != nil {
			logger.Warn("dcgm-init failed to shut down DCGM client cleanly", logger.Fields{"error": err})
		}
	}()

	return e.run(ctx)
}

// run collects metrics immediately and then on every tick of
// metricsCollectionInterval until the context is cancelled.
func (e *Engine) run(ctx context.Context) error {
	ticker := time.NewTicker(metricsCollectionInterval)
	defer ticker.Stop()

	// Collect once up front so the file is populated without waiting a full
	// interval. A failure here is not fatal; the ticker will retry.
	if err := e.collectAndWrite(ctx); err != nil {
		logger.Warn("dcgm-init initial metrics collection failed, will retry", logger.Fields{"error": err})
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if err := e.collectAndWrite(ctx); err != nil {
				logger.Warn("dcgm-init metrics collection failed", logger.Fields{"error": err})
			}
		}
	}
}

// metricsOutput is the JSON structure written to the shared metrics file. Its
// shape and tags must match what the agent reads.
type metricsOutput struct {
	Timestamp       string `json:"timestamp"`
	Healthy         bool   `json:"healthy"`
	UnhealthyReason string `json:"unhealthy_reason,omitempty"`
	// ConnectionLost indicates the DCGM/nv-hostengine connection is lost
	// (outside the grace period). When true, the reader cannot determine GPU
	// health and should report INSUFFICIENT_DATA rather than trusting Healthy:
	// IsHealthy() returns true when disconnected (it only flips to false on a
	// known violation/FAIL), so Healthy alone is not sufficient.
	ConnectionLost bool                 `json:"connection_lost,omitempty"`
	GPUs           []gputypes.GPUMetric `json:"gpus"`
}

// collectAndWrite reconciles the DCGM connection, pulls the latest metrics from
// the client, and writes them to the shared output file atomically: it stages
// the data into the temp file and then renames it onto the final path so a
// reader (the agent) never observes a partially written file.
//
// A failure to reconcile or collect is not fatal: we still write a fresh status
// snapshot reflecting the current health and connection state so the shared
// file does not silently go stale. Only a marshal or write/rename failure is
// returned as an error.
func (e *Engine) collectAndWrite(ctx context.Context) error {
	if _, err := e.client.Reconcile(ctx); err != nil {
		logger.Warn("dcgm-init DCGM reconciliation failed, skipping metrics collection", logger.Fields{"error": err})
		return nil
	}

	metrics, err := e.client.GetMetrics(ctx)
	if err != nil {
		// Keep going with no per-GPU metrics; the health/connection fields (implemented later can convey information)
		logger.Warn("dcgm-init failed to collect GPU metrics, writing status only", logger.Fields{"error": err})
		metrics = []gputypes.GPUMetric{}
	}

	output := metricsOutput{
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		Healthy:         e.client.IsHealthy(),
		UnhealthyReason: e.client.UnhealthyReason(),
		ConnectionLost:  e.client.IsConnectionLost(),
		GPUs:            metrics,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}

	// Write to the temp file and then atomically rename it onto the final path.
	// The reader always consumes MetricsFilePath; TempMetricsFilePath is only
	// the transient write target that the rename moves into place.
	if err := os.WriteFile(TempMetricsFilePath, data, metricsFilePermission); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", TempMetricsFilePath, err)
	}
	if err := os.Rename(TempMetricsFilePath, MetricsFilePath); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", TempMetricsFilePath, MetricsFilePath, err)
	}

	logger.Debug("metrics written", logger.Fields{"path": MetricsFilePath, "gpuCount": len(output.GPUs)})
	return nil
}

// ensureFile creates path with metricsFilePermission if it does not already
// exist. An existing file is left untouched so a restart preserves the last
// metrics written.
func ensureFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE, metricsFilePermission)
	if err != nil {
		return err
	}
	return f.Close()
}
