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
	"github.com/aws/amazon-ecs-agent/dcgm-init/logger"
	log "github.com/cihub/seelog"
)

// Supported commands
const (
	START = "start"
	STOP  = "stop"
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
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	action, ok := actions()[args[0]]
	if !ok {
		usage()
		os.Exit(1)
	}

	if action.immediate {
		os.Exit(0)
	}

	logger.Setup()
	defer log.Flush()

	config := gpu.Config{
		SocketPath:                *socketPath,
		InitializationGracePeriod: gpu.DefaultInitializationGracePeriod,
	}

	client := gpu.NewClient(config)
	defer client.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("dcgm-init received shutdown signal")
		cancel()
	}()

	// Create the output directory with restrictive permissions (owner: root, mode: 0755).
	// Only dcgm-init (running as root) can write; ecs-agent reads via read-only bind mount.
	outputDir := filepath.Dir(*outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		die(fmt.Sprintf("failed to create output directory %s: %v", outputDir, err))
	}

	if err := run(ctx, client, *outputPath, *collectionFreq, *oneShot); err != nil {
		die(fmt.Sprintf("dcgm-init failed: %v", err))
	}
}

func run(ctx context.Context, client gpu.Client, outputPath string, interval time.Duration, oneShot bool) error {
	reinitialized, err := client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("initial DCGM reconciliation failed: %w", err)
	}
	log.Infof("DCGM client reconciled: reinitialized=%v", reinitialized)

	if oneShot {
		return collectAndWrite(ctx, client, outputPath)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := collectAndWrite(ctx, client, outputPath); err != nil {
		log.Warnf("dcgm-init initial collection failed, will retry: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if _, err := client.Reconcile(ctx); err != nil {
				log.Warnf("dcgm-init client reconciliation failed: %v", err)
				continue
			}
			if err := collectAndWrite(ctx, client, outputPath); err != nil {
				log.Warnf("dcgm-init metrics collection failed: %v", err)
			}
		}
	}
}

type metricsOutput struct {
	Timestamp string          `json:"timestamp"`
	GPUs      []gpuMetricJSON `json:"gpus"`
}

type gpuMetricJSON struct {
	GPUUUID            string   `json:"gpu_uuid"`
	GPUUtilization     *float64 `json:"gpu_utilization_percent,omitempty"`
	MemoryUtilization  *float64 `json:"memory_utilization_percent,omitempty"`
	MemoryTotal        *uint64  `json:"memory_total_bytes,omitempty"`
	MemoryUsed         *uint64  `json:"memory_used_bytes,omitempty"`
	PowerDraw          *float64 `json:"power_draw_watts,omitempty"`
	Temperature        *float64 `json:"temperature_celsius,omitempty"`
	RestartAppXidCount int64    `json:"restart_app_xid_count"`
}

func collectAndWrite(ctx context.Context, client gpu.Client, outputPath string) error {
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

	log.Infof("metrics written: path=%s, gpuCount=%d", outputPath, len(gpus))
	return nil
}

type action struct {
	description string
	immediate   bool // if true, exit immediately (no daemon work needed)
}

func actions() map[string]action {
	return map[string]action{
		START: {
			description: "Start collecting GPU metrics",
			immediate:   false,
		},
		STOP: {
			description: "Stop collecting GPU metrics (handled by SIGTERM)",
			immediate:   true,
		},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] COMMAND\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, " Available commands:\n")
	for cmd, a := range actions() {
		fmt.Fprintf(os.Stderr, "  %-10s  %s\n", cmd, a.description)
	}
	fmt.Fprintf(os.Stderr, "\n")
}

func die(msg string) {
	log.Error(msg)
	log.Flush()
	os.Exit(1)
}
