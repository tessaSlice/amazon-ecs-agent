//go:build linux

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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/amazon-ecs-agent/dcgm-init/gpu"
	"go.uber.org/zap"
)

const (
	defaultOutputPath     = "/var/run/ecs/gpu-metrics.json"
	defaultCollectionFreq = 60 * time.Second
)

func main() {
	socketPath := flag.String("socket-path", gpu.DefaultSocketPath, "Path to the DCGM nv-hostengine Unix domain socket")
	outputPath := flag.String("output", defaultOutputPath, "Path to write GPU metrics JSON output")
	collectionFreq := flag.Duration("interval", defaultCollectionFreq, "Metrics collection interval")
	oneShot := flag.Bool("once", false, "Collect metrics once and exit")
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "start":
			// Continue to normal daemon operation below.
		case "stop":
			// Stop is handled by systemd sending SIGTERM; nothing to do here.
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s (use 'start' or 'stop')\n", args[0])
			os.Exit(1)
		}
	}

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	config := gpu.Config{
		SocketPath:                *socketPath,
		InitializationGracePeriod: gpu.DefaultInitializationGracePeriod,
	}

	client := gpu.NewClient(config, logger)
	defer client.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("received shutdown signal")
		cancel()
	}()

	// Create the output directory with restrictive permissions (owner: root, mode: 0755).
	// Only dcgm-init (running as root) can write; ecs-agent reads via read-only bind mount.
	outputDir := filepath.Dir(*outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		logger.Error("failed to create output directory", zap.String("path", outputDir), zap.Error(err))
		os.Exit(1)
	}

	if err := run(ctx, client, logger, *outputPath, *collectionFreq, *oneShot); err != nil {
		logger.Error("dcgm-init failed", zap.Error(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, client gpu.Client, logger *zap.Logger, outputPath string, interval time.Duration, oneShot bool) error {
	reinitialized, err := client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("initial DCGM reconciliation failed: %w", err)
	}
	logger.Info("DCGM client reconciled", zap.Bool("reinitialized", reinitialized))

	if oneShot {
		return collectAndWrite(ctx, client, logger, outputPath)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := collectAndWrite(ctx, client, logger, outputPath); err != nil {
		logger.Warn("initial collection failed, will retry", zap.Error(err))
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down metrics collection")
			return nil
		case <-ticker.C:
			if _, err := client.Reconcile(ctx); err != nil {
				logger.Warn("reconciliation failed", zap.Error(err))
				continue
			}
			if err := collectAndWrite(ctx, client, logger, outputPath); err != nil {
				logger.Warn("metrics collection failed", zap.Error(err))
			}
		}
	}
}

type metricsOutput struct {
	Timestamp string           `json:"timestamp"`
	GPUs      []gpuMetricJSON  `json:"gpus"`
	Healthy   bool             `json:"healthy"`
	Reason    string           `json:"unhealthy_reason,omitempty"`
}

type gpuMetricJSON struct {
	GPUUUID           string   `json:"gpu_uuid"`
	GPUUtilization    *float64 `json:"gpu_utilization_percent,omitempty"`
	MemoryUtilization *float64 `json:"memory_utilization_percent,omitempty"`
	MemoryTotal       *uint64  `json:"memory_total_bytes,omitempty"`
	MemoryUsed        *uint64  `json:"memory_used_bytes,omitempty"`
	PowerDraw         *float64 `json:"power_draw_watts,omitempty"`
	Temperature       *float64 `json:"temperature_celsius,omitempty"`
	RestartAppXidCount int64   `json:"restart_app_xid_count"`
}

func collectAndWrite(ctx context.Context, client gpu.Client, logger *zap.Logger, outputPath string) error {
	metrics, err := client.GetMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect GPU metrics: %w", err)
	}

	gpus := make([]gpuMetricJSON, len(metrics))
	for i, m := range metrics {
		gpus[i] = gpuMetricJSON{
			GPUUUID:            m.GPUUUID,
			GPUUtilization:     m.GPUUtilization,
			MemoryUtilization:  m.MemoryUtilization,
			MemoryTotal:        m.MemoryTotal,
			MemoryUsed:         m.MemoryUsed,
			PowerDraw:          m.PowerDraw,
			Temperature:        m.Temperature,
			RestartAppXidCount: m.RestartAppXidCount,
		}
	}

	output := metricsOutput{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs:      gpus,
		Healthy:   client.IsHealthy(),
		Reason:    client.UnhealthyReason(),
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}

	tmpPath := outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", tmpPath, err)
	}

	stagingPath := outputPath + ".staging"
	if err := os.WriteFile(stagingPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", stagingPath, err)
	}
	if err := os.Rename(stagingPath, outputPath); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", stagingPath, outputPath, err)
	}

	logger.Info("metrics written", zap.String("path", outputPath), zap.Int("gpuCount", len(gpus)))
	return nil
}
