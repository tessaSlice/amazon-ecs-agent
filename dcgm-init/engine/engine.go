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
	// DefaultInitErrorExitCode is the default error exit code
	DefaultInitErrorExitCode = -1
)

const (
	metricsFilePermission    os.FileMode = 0644
	metricsDirPermission     os.FileMode = 0755
	metricsFileTempSuffix                = ".tmp"
	metricsCollectionInterval            = 60 * time.Second
)

// Engine collects GPU metrics via dcgm.Client and writes them to a shared
// JSON file the agent reads.
type Engine struct {
	client             dcgm.Client
	outputPath         string
	collectionInterval time.Duration
}

func New() (*Engine, error) {
	return &Engine{
		client:             dcgm.NewClient(dcgm.Config{}),
		outputPath:         gputypes.GPUMetricsFilePath,
		collectionInterval: metricsCollectionInterval,
	}, nil
}

func (e *Engine) tempPath() string {
	return e.outputPath + metricsFileTempSuffix
}

// Start creates the metrics dir and files, then runs the collection loop
// until SIGTERM.
func (e *Engine) Start() error {
	outputDir := filepath.Dir(e.outputPath)
	if err := os.MkdirAll(outputDir, metricsDirPermission); err != nil {
		return fmt.Errorf("dcgm-init cannot create metrics directory %s: %w", outputDir, err)
	}

	for _, path := range []string{e.outputPath, e.tempPath()} {
		if err := ensureCreatable(path); err != nil {
			return fmt.Errorf("dcgm-init cannot create or write metrics file %s: %w", path, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	defer func() {
		if err := e.client.Shutdown(); err != nil {
			logger.Warn("dcgm-init failed to shut down DCGM client cleanly", logger.Fields{"error": err})
		}
	}()

	e.run(ctx)
	return nil
}

func (e *Engine) run(ctx context.Context) {
	ticker := time.NewTicker(e.collectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return
		case <-ticker.C:
			if err := e.reconcileAndCollect(ctx); err != nil {
				logger.Warn("dcgm-init metrics collection failed", logger.Fields{"error": err})
			}
		}
	}
}

// reconcileAndCollect reconnects if needed, collects metrics, and writes
// atomically (temp + rename). Reconcile/collect failures are non-fatal: a
// status-only snapshot is written so the agent knows dcgm-init is alive.
func (e *Engine) reconcileAndCollect(ctx context.Context) error {
	metrics := []gputypes.GPUMetric{}
	if _, err := e.client.Reconcile(ctx); err != nil {
		logger.Warn("dcgm-init DCGM reconciliation failed, skipping metrics collection", logger.Fields{"error": err})
	} else {
		collected, err := e.client.GetMetrics(ctx)
		if err != nil {
			logger.Warn("dcgm-init failed to collect GPU metrics, writing status only", logger.Fields{"error": err})
		} else {
			metrics = collected
		}
	}

	output := gputypes.GPUMetricsFileData{
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

	tempPath := e.tempPath()
	if err := os.WriteFile(tempPath, data, metricsFilePermission); err != nil {
		return fmt.Errorf("dcgm-init failed to write metrics to %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, e.outputPath); err != nil {
		return fmt.Errorf("dcgm-init failed to rename %s to %s: %w", tempPath, e.outputPath, err)
	}
	return nil
}

func ensureCreatable(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, metricsFilePermission)
	if err != nil {
		return err
	}
	return f.Close()
}
