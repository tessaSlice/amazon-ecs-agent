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

	"github.com/aws/amazon-ecs-agent/dcgm-init/gpu"
	log "github.com/cihub/seelog"
)

const (
	DefaultOutputPath     = "/var/run/ecs/gpu-metrics.json"
	DefaultCollectionFreq = 60 * time.Second
)

// Engine contains methods invoked when dcgm-init is run.
type Engine struct {
	client         gpu.Client
	outputPath     string
	collectionFreq time.Duration
	oneShot        bool
}

// New creates an instance of Engine with the given configuration.
func New(socketPath string, outputPath string, collectionFreq time.Duration, oneShot bool) *Engine {
	config := gpu.Config{
		SocketPath:                socketPath,
		InitializationGracePeriod: gpu.DefaultInitializationGracePeriod,
	}

	return &Engine{
		client:         gpu.NewClient(config),
		outputPath:     outputPath,
		collectionFreq: collectionFreq,
		oneShot:        oneShot,
	}
}

// Start connects to DCGM and runs the metrics collection loop until
// a SIGTERM/SIGINT is received. This is the main entry point for the
// "start" command.
func (e *Engine) Start() error {
	defer e.client.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("dcgm-init received shutdown signal")
		cancel()
	}()

	outputDir := filepath.Dir(e.outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
	}

	return e.run(ctx)
}

// Stop is a no-op. The running dcgm-init process is stopped via SIGTERM
// from systemd, not by this command. This exists for symmetry with the
// systemd unit lifecycle.
func (e *Engine) Stop() error {
	return nil
}

func (e *Engine) run(ctx context.Context) error {
	reinitialized, err := e.client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("initial DCGM reconciliation failed: %w", err)
	}
	log.Infof("DCGM client reconciled: reinitialized=%v", reinitialized)

	if e.oneShot {
		return e.collectAndWrite(ctx)
	}

	ticker := time.NewTicker(e.collectionFreq)
	defer ticker.Stop()

	if err := e.collectAndWrite(ctx); err != nil {
		log.Warnf("dcgm-init initial collection failed, will retry: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if _, err := e.client.Reconcile(ctx); err != nil {
				log.Warnf("dcgm-init client reconciliation failed: %v", err)
				continue
			}
			if err := e.collectAndWrite(ctx); err != nil {
				log.Warnf("dcgm-init metrics collection failed: %v", err)
			}
		}
	}
}

type metricsOutput struct {
	Timestamp       string          `json:"timestamp"`
	Healthy         bool            `json:"healthy"`
	UnhealthyReason string          `json:"unhealthy_reason,omitempty"`
	GPUs            []gpuMetricJSON `json:"gpus"`
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

func (e *Engine) collectAndWrite(ctx context.Context) error {
	metrics, err := e.client.GetMetrics(ctx)
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
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		Healthy:         e.client.IsHealthy(),
		UnhealthyReason: e.client.UnhealthyReason(),
		GPUs:            gpus,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}

	tmpPath := e.outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", tmpPath, err)
	}

	stagingPath := e.outputPath + ".staging"
	if err := os.WriteFile(stagingPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", stagingPath, err)
	}
	if err := os.Rename(stagingPath, e.outputPath); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", stagingPath, e.outputPath, err)
	}

	log.Infof("metrics written: path=%s, gpuCount=%d", e.outputPath, len(gpus))
	return nil
}
