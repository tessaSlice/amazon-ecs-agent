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
	"errors"
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
	// outputPath is the shared file where dcgm-init writes GPU metrics for the
	// agent to consume. It must match the path the agent reads from.
	outputPath = "/var/run/ecs/gpu-metrics.json"

	// collectionFreq is the interval between metrics collections.
	collectionFreq = 60 * time.Second

	// outputDirPermission is the permission for the directory holding the metrics file.
	outputDirPermission = 0755

	// outputFilePermission is the permission for the metrics file (and its staging copy).
	outputFilePermission = 0644

	// unixWOK is the write-permission mode passed to syscall.Access to test
	// whether the current process can write into the output directory (W_OK).
	unixWOK = 0x2
)

// Exit codes for the dcgm-init binary. They let the dcgm-init systemd unit's
// RestartPreventExitStatus=5 distinguish an unrecoverable failure — which a
// restart cannot fix — from a transient failure that systemd should retry.
const (
	// RestartPreventExitCode signals an unrecoverable failure. Because the unit
	// sets RestartPreventExitStatus=5, exiting with this code prevents systemd
	// from restarting dcgm-init in a hot loop over a problem a restart won't fix.
	RestartPreventExitCode = 5

	// DefaultErrorExitCode signals a transient/generic failure. With
	// Restart=on-failure, systemd will restart the service after RestartSec.
	DefaultErrorExitCode = 1
)

// ErrOutputDirUnusable is returned (wrapped) when the metrics output directory
// cannot be used — it does not exist and cannot be created, or it exists but is
// not a writable directory. It is a configuration problem a restart cannot fix,
// so main() maps it to RestartPreventExitCode. Callers wrap it with fmt.Errorf
// %w and detect it with errors.Is; it is never surfaced for its text (each call
// site supplies its own descriptive prefix), so the message is kept terse.
var ErrOutputDirUnusable = errors.New("output directory unusable")

// Filesystem operations used by ensureOutputDir and collectAndWrite, indirected
// through package vars so tests can deterministically exercise each branch and
// redirect I/O away from the fixed outputPath const (a real MkdirAll/write
// failure is permission-dependent and would not reproduce under root). Production
// code uses the real os/syscall implementations.
var (
	osStat      = os.Stat
	osMkdirAll  = os.MkdirAll
	checkAccess = syscall.Access
	osWriteFile = os.WriteFile
	osRename    = os.Rename
	osRemove    = os.Remove
)

// Engine drives the dcgm-init metrics collection loop: it connects to DCGM via
// the dcgm.Client, periodically collects GPU metrics, and writes them to a shared
// JSON file that the agent reads. Its configuration (outputPath, collectionFreq)
// is fixed at compile time via package constants rather than command-line flags.
type Engine struct {
	client dcgm.Client
}

// New creates an Engine backed by a DCGM client. An empty dcgm.Config is passed
// so NewClient applies its own defaults (DefaultSocketPath,
// DefaultInitializationGracePeriod) rather than restating them here.
func New() *Engine {
	return &Engine{
		client: dcgm.NewClient(dcgm.Config{}),
	}
}

// Start connects to DCGM and runs the metrics collection loop until
// a SIGTERM/SIGINT is received. This is the main entry point for the
// "start" command.
//
// Shutdown is driven entirely by signals: systemd's default stop sends SIGTERM
// to the process, which the watcher below turns into a context cancellation that
// unwinds the run loop. There is no separate "stop" command, so the unit needs
// no ExecStop.
func (e *Engine) Start() error {
	defer func() {
		if err := e.client.Shutdown(); err != nil {
			logger.Warn("dcgm-init failed to shut down DCGM client cleanly", logger.Fields{"error": err})
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// Stop delivering signals to sigCh once we return so the process's signal
	// disposition is restored and the watcher goroutine below can exit.
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			logger.Info("dcgm-init received shutdown signal")
			cancel()
		case <-ctx.Done():
			// run() returned (e.g. one-shot mode); unblock and exit rather
			// than leaking this goroutine for the life of the process.
		}
	}()

	if err := e.ensureOutputDir(); err != nil {
		return err
	}

	return e.run(ctx)
}

// ensureOutputDir makes sure the directory holding the metrics file exists and
// is writable by this (dcgm-init, host-side) process before the collection loop
// starts. The directory is normally pre-created by the ecs-init package install
// / a systemd-tmpfiles rule, so the common path is just a stat + writability
// check; MkdirAll is only a fallback for when it is genuinely absent (e.g. the
// tmpfs entry was not recreated). A path that exists but is not a writable
// directory is a configuration problem a restart cannot fix, so it is returned
// wrapping ErrOutputDirUnusable to stop systemd restart-looping.
func (e *Engine) ensureOutputDir() error {
	outputDir := filepath.Dir(outputPath)

	info, statErr := osStat(outputDir)
	if statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("output path %s exists but is not a directory: %w", outputDir, ErrOutputDirUnusable)
		}
		// Directory exists — confirm we can write into it rather than assuming.
		if accessErr := checkAccess(outputDir, unixWOK); accessErr != nil {
			return fmt.Errorf("output directory %s is not writable: %w: %w", outputDir, accessErr, ErrOutputDirUnusable)
		}
		return nil
	}
	if !os.IsNotExist(statErr) {
		return fmt.Errorf("failed to stat output directory %s: %w: %w", outputDir, statErr, ErrOutputDirUnusable)
	}

	// Directory does not exist — create it as a fallback.
	if err := osMkdirAll(outputDir, outputDirPermission); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w: %w", outputDir, err, ErrOutputDirUnusable)
	}
	return nil
}

// run reconciles the DCGM connection, then collects and writes metrics on each
// tick of collectionFreq until the context is cancelled.
func (e *Engine) run(ctx context.Context) error {
	reinitialized, err := e.client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("initial DCGM reconciliation failed: %w", err)
	}
	logger.Info("DCGM client reconciled", logger.Fields{"reinitialized": reinitialized})

	ticker := time.NewTicker(collectionFreq)
	defer ticker.Stop()

	// If a shutdown signal arrived during startup (e.g. while Reconcile was
	// blocking), skip the initial collection so we don't write during teardown.
	if ctx.Err() != nil {
		logger.Info("dcgm-init is shutting down before initial metrics collection")
		return nil
	}
	if err := e.collectAndWrite(ctx); err != nil {
		logger.Warn("dcgm-init initial collection failed, will retry", logger.Fields{"error": err})
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if _, err := e.client.Reconcile(ctx); err != nil {
				logger.Warn("dcgm-init client reconciliation failed", logger.Fields{"error": err})
				// Fall through and still write a status update so the shared
				// file reflects the current (likely connection-lost) health
				// with a fresh timestamp rather than silently going stale.
			}
			if err := e.collectAndWrite(ctx); err != nil {
				logger.Warn("dcgm-init metrics collection failed", logger.Fields{"error": err})
			}
		}
	}
}

// metricsOutput is the JSON structure written to the shared metrics file.
// Its shape and tags must match what the agent reads.
type metricsOutput struct {
	Timestamp       string `json:"timestamp"`
	Healthy         bool   `json:"healthy"`
	UnhealthyReason string `json:"unhealthy_reason,omitempty"`
	// ConnectionLost indicates the DCGM/nv-hostengine connection is lost (outside
	// the grace period). When true, the reader cannot determine GPU health and
	// should report INSUFFICIENT_DATA rather than trusting Healthy: IsHealthy()
	// returns true when disconnected (it only flips to false on a known
	// violation/FAIL), so Healthy alone is not sufficient.
	ConnectionLost bool                 `json:"connection_lost,omitempty"`
	GPUs           []gputypes.GPUMetric `json:"gpus"`
}

// collectAndWrite pulls the latest metrics from the DCGM client and writes them
// to the shared output file. The write is atomic: data is written to a staging
// file and then renamed onto the final path so readers never observe a partially
// written file.
//
// A failure to collect metrics (e.g. the DCGM connection is down) is not fatal:
// we still write a fresh status snapshot reflecting the current health and
// connection state so the shared file does not silently go stale and one-shot
// runs still produce an output file. Only a marshal or write/rename failure is
// returned as an error.
func (e *Engine) collectAndWrite(ctx context.Context) error {
	metrics, err := e.client.GetMetrics(ctx)
	if err != nil {
		// Keep going with no per-GPU metrics; the health/connection fields below
		// still convey the current state to the reader with a fresh timestamp.
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

	// Write to a temporary file and then atomically rename it onto the final
	// path so a concurrent reader (the agent) never observes a partially written
	// file. The reader always consumes outputPath; outputPath+".tmp" is only
	// the transient write target that the rename moves into place.
	tmpPath := outputPath + ".tmp"
	if err := osWriteFile(tmpPath, data, outputFilePermission); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", tmpPath, err)
	}
	if err := osRename(tmpPath, outputPath); err != nil {
		// Best-effort cleanup so a failed rename does not leave an orphaned
		// temp file behind on every collection tick.
		osRemove(tmpPath)
		return fmt.Errorf("failed to rename %s to %s: %w", tmpPath, outputPath, err)
	}

	logger.Info("metrics written", logger.Fields{"path": outputPath, "gpuCount": len(output.GPUs)})
	return nil
}
