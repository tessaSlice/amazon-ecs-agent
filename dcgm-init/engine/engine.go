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
	"path/filepath"
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
	// MetricsFilePath is the shared file dcgm-init writes and the agent reads.
	// /var/run/ecs is tmpfs, so Start MkdirAll's the dir each boot rather than
	// relying on the RPM or a tmpfiles rule.
	MetricsFilePath = "/var/run/ecs/gpu-metrics.json"

	// 0755 so the agent can traverse the bind-mounted dir to read the file.
	metricsDirPermission os.FileMode = 0755

	// 0644 so the file is world-readable for the agent; only dcgm-init writes it.
	metricsFilePermission os.FileMode = 0644

	// metricsCollectionInterval matches the agent's sampling cadence.
	metricsCollectionInterval = 60 * time.Second
)

// Engine periodically collects GPU metrics from DCGM and writes them to a shared
// JSON file the agent reads. outputPath and collectionInterval default to the
// package constants; tests override them to redirect writes and shrink the ticker.
type Engine struct {
	client             dcgm.Client
	outputPath         string
	collectionInterval time.Duration
}

// New creates an Engine. The DCGM client connects lazily on the first Reconcile,
// so New is cheap and does not fail when nv-hostengine is not yet up.
func New() (*Engine, error) {
	return &Engine{
		client:             dcgm.NewClient(dcgm.Config{}),
		outputPath:         MetricsFilePath,
		collectionInterval: metricsCollectionInterval,
	}, nil
}

// tempPath is the staging file that reconcileAndCollect writes before atomically
// renaming it onto outputPath.
func (e *Engine) tempPath() string {
	return e.outputPath + ".tmp"
}

// Start runs the collection loop until SIGTERM/SIGINT (systemd's default stop
// sends SIGTERM); there is no separate "stop" command.
func (e *Engine) Start() error {
	// Create the metrics dir at runtime (not shipped in the RPM); no-op if present.
	metricsDir := filepath.Dir(e.outputPath)
	if err := os.MkdirAll(metricsDir, metricsDirPermission); err != nil {
		return fmt.Errorf("dcgm-init could not create metrics directory %s: %w", metricsDir, err)
	}

	// Fail fast if the metrics files can't be opened, rather than failing every
	// tick. ensureWritable creates them if missing.
	for _, path := range []string{e.outputPath, e.tempPath()} {
		if err := ensureWritable(path); err != nil {
			return fmt.Errorf("dcgm-init required metrics file %s is missing or not writable: %w", path, err)
		}
	}

	// A shutdown signal cancels ctx, unwinding the run loop.
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
	ticker := time.NewTicker(e.collectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if err := e.reconcileAndCollect(ctx); err != nil {
				logger.Warn("dcgm-init metrics collection failed", logger.Fields{"error": err})
			}
		}
	}
}

// dcgmOutput is the JSON structure written to the shared metrics file. Its
// shape and tags must match what the agent reads.
type dcgmOutput struct {
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

// reconcileAndCollect reconciles the DCGM connection, collects the latest
// metrics, and writes them to the output file via a temp-file+rename so a reader
// never sees a partial file. A reconcile/collect failure is not fatal — it still
// writes a status snapshot so the file doesn't go stale; only marshal or
// write/rename failures are returned.
func (e *Engine) reconcileAndCollect(ctx context.Context) error {
	// On reconcile/collect failure, fall through with empty metrics so the
	// snapshot still records fresh ConnectionLost/Healthy fields.
	metrics := []gputypes.GPUMetric{}
	if _, err := e.client.Reconcile(ctx); err != nil {
		logger.Warn("dcgm-init DCGM reconciliation failed, writing status only", logger.Fields{"error": err})
	} else if collected, err := e.client.GetMetrics(ctx); err != nil {
		logger.Warn("dcgm-init failed to collect GPU metrics, writing status only", logger.Fields{"error": err})
	} else {
		metrics = collected
	}

	output := dcgmOutput{
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		Healthy:         e.client.IsHealthy(),
		UnhealthyReason: e.client.UnhealthyReason(),
		ConnectionLost:  e.client.IsConnectionLost(),
		GPUs:            metrics,
	}
	logger.Debug("dcgm-init collected GPU metrics", logger.Fields{"path": e.outputPath, "gpuCount": len(output.GPUs)})

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("dcgm-init failed to marshal metrics: %w", err)
	}

	// Stage into the temp file, then atomically rename onto outputPath.
	tempPath := e.tempPath()
	if err := os.WriteFile(tempPath, data, metricsFilePermission); err != nil {
		return fmt.Errorf("dcgm-init failed to write metrics to %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, e.outputPath); err != nil {
		return fmt.Errorf("dcgm-init failed to rename %s to %s: %w", tempPath, e.outputPath, err)
	}

	logger.Debug("dcgm-init wrote GPU metrics")
	return nil
}

// ensureWritable opens path with O_CREATE (not O_TRUNC): missing files are
// created empty, existing ones left intact, dir/permission problems surface as
// errors. Creating matters because reconcileAndCollect's rename consumes the
// .tmp file, so it is absent on a later Start.
func ensureWritable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, metricsFilePermission)
	if err != nil {
		return err
	}
	return f.Close()
}
