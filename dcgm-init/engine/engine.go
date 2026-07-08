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
	// socketPath is the DCGM nv-hostengine Unix domain socket dcgm-init connects to.
	socketPath = dcgm.DefaultSocketPath

	// outputPath is the shared file where dcgm-init writes GPU metrics for the
	// agent to consume. It must match the path the agent reads from.
	outputPath = "/var/run/ecs/gpu-metrics.json"

	// collectionFreq is the interval between metrics collections.
	collectionFreq = 60 * time.Second

	// outputDirPermission is the permission for the directory holding the metrics file.
	outputDirPermission = 0755

	// outputFilePermission is the permission for the metrics file (and its staging copy).
	outputFilePermission = 0644

	// unixWOK is the write-permission mode (W_OK) passed to syscall.Access to
	// test whether this process can write into the output directory.
	unixWOK = 0x2

	// maxConsecutiveFailures is the number of consecutive per-tick failures
	// (persistent DCGM reconciliation loss, or metrics write failures) after
	// which run() gives up and returns an error so the process exits non-zero
	// and systemd restarts it, rather than looping forever while the shared
	// file silently goes stale.
	maxConsecutiveFailures = 5
)

// ErrSetup marks a startup/configuration failure (e.g. creating the output
// directory or the initial DCGM reconciliation). main() maps errors wrapping it
// to ExitSetupError; all other failures map to a generic runtime exit code.
var ErrSetup = errors.New("dcgm-init setup failed")

// Engine drives the dcgm-init metrics collection loop: it connects to DCGM via
// the dcgm.Client, periodically collects GPU metrics, and writes them to a shared
// JSON file that the agent reads.
type Engine struct {
	client dcgm.Client
}

// New creates an Engine backed by a DCGM client.
func New() *Engine {
	config := dcgm.Config{
		SocketPath:                socketPath,
		InitializationGracePeriod: dcgm.DefaultInitializationGracePeriod,
	}
	return &Engine{
		client: dcgm.NewClient(config),
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
			// run() returned (e.g. a setup failure); unblock and exit rather
			// than leaking this goroutine for the life of the process.
		}
	}()

	if err := ensureOutputDir(); err != nil {
		return err
	}

	return e.run(ctx)
}

// ensureOutputDir makes sure the directory holding the metrics file exists and
// is writable by this process before (and during) the collection loop. The
// directory is normally pre-created by the package install / a systemd-tmpfiles
// rule, so the common path is a stat + writability check; MkdirAll is only a
// fallback for when it is genuinely absent (e.g. a tmpfs entry that was not
// recreated). A path that exists but is not a writable directory is a
// configuration problem a restart cannot fix, so it is returned wrapping
// ErrSetup, which main() maps to ExitSetupError.
func ensureOutputDir() error {
	outputDir := filepath.Dir(outputPath)

	info, statErr := os.Stat(outputDir)
	if statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: output path %s exists but is not a directory", ErrSetup, outputDir)
		}
		// Directory exists — confirm we can write into it rather than assuming.
		if accessErr := syscall.Access(outputDir, unixWOK); accessErr != nil {
			return fmt.Errorf("%w: output directory %s is not writable: %w", ErrSetup, outputDir, accessErr)
		}
		return nil
	}
	if !os.IsNotExist(statErr) {
		return fmt.Errorf("%w: failed to stat output directory %s: %w", ErrSetup, outputDir, statErr)
	}

	// Directory does not exist — create it as a fallback.
	if err := os.MkdirAll(outputDir, outputDirPermission); err != nil {
		return fmt.Errorf("%w: failed to create output directory %s: %w", ErrSetup, outputDir, err)
	}
	return nil
}

// run reconciles the DCGM connection, then collects and writes metrics on each
// tick of collectionFreq until the context is cancelled.
func (e *Engine) run(ctx context.Context) error {
	reinitialized, err := e.client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("%w: initial DCGM reconciliation failed: %w", ErrSetup, err)
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
	// A single failure is expected and non-fatal (transient DCGM hiccup, brief
	// connection loss). But a run that can never make progress — a permanently
	// unwritable output path, a deleted output directory, or a dead nv-hostengine
	// past the grace period — must not loop forever silently. We track two
	// distinct persistent-failure modes and, once either crosses
	// maxConsecutiveFailures, return an error so the process exits non-zero and
	// systemd restarts it (a restart may recover a transient issue and at least
	// surfaces a permanent one instead of hiding it). Any success resets the
	// corresponding counter.
	//
	// The two are tracked separately because a lost DCGM connection and a failed
	// file write are independent: collectAndWrite still returns nil (it writes a
	// status-only snapshot) when DCGM is down, so a dead nv-hostengine would never
	// trip the write counter — the reconcile counter catches that case.
	consecutiveReconcileFailures := 0
	consecutiveWriteFailures := 0

	if err := e.collectAndWrite(ctx); err != nil {
		consecutiveWriteFailures++
		logger.Warn("dcgm-init initial collection failed, will retry", logger.Fields{"error": err})
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-ticker.C:
			if _, err := e.client.Reconcile(ctx); err != nil {
				consecutiveReconcileFailures++
				logger.Warn("dcgm-init client reconciliation failed", logger.Fields{
					"error":               err,
					"consecutiveFailures": consecutiveReconcileFailures,
				})
				// Fall through and still write a status update so the shared
				// file reflects the current (likely connection-lost) health
				// with a fresh timestamp rather than silently going stale.
				if consecutiveReconcileFailures >= maxConsecutiveFailures {
					return fmt.Errorf("dcgm-init giving up after %d consecutive DCGM reconciliation failures: %w",
						consecutiveReconcileFailures, err)
				}
			} else {
				consecutiveReconcileFailures = 0
			}
			if err := e.collectAndWrite(ctx); err != nil {
				consecutiveWriteFailures++
				logger.Warn("dcgm-init metrics collection failed", logger.Fields{
					"error":               err,
					"consecutiveFailures": consecutiveWriteFailures,
				})
				if consecutiveWriteFailures >= maxConsecutiveFailures {
					return fmt.Errorf("dcgm-init giving up after %d consecutive metrics write failures: %w",
						consecutiveWriteFailures, err)
				}
			} else {
				consecutiveWriteFailures = 0
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

// getMetrics calls the DCGM client's GetMetrics but honors ctx cancellation.
// The client's GetMetrics performs synchronous cgo calls into nv-hostengine and
// does not itself observe ctx, so a wedged nv-hostengine could otherwise block
// the collection tick indefinitely and stall signal-driven shutdown. Running it
// in a goroutine lets us return promptly on ctx.Done(); the goroutine (and the
// blocked cgo call) is abandoned but the buffered channel ensures it does not
// leak on the normal completion path.
func (e *Engine) getMetrics(ctx context.Context) ([]gputypes.GPUMetric, error) {
	type result struct {
		metrics []gputypes.GPUMetric
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		metrics, err := e.client.GetMetrics(ctx)
		resCh <- result{metrics: metrics, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resCh:
		return res.metrics, res.err
	}
}

// collectAndWrite pulls the latest metrics from the DCGM client and writes them
// to the shared output file. The write is atomic: data is written to a staging
// file and then renamed onto the final path so readers never observe a partially
// written file.
//
// A failure to collect metrics (e.g. the DCGM connection is down) is not fatal:
// we still write a fresh status snapshot reflecting the current health and
// connection state so the shared file does not silently go stale. Only a marshal
// or write/rename failure is returned as an error.
func (e *Engine) collectAndWrite(ctx context.Context) error {
	// Re-ensure the output directory on every collection: it is created once in
	// Start(), but /var/run is tmpfs and the directory can be pruned at runtime
	// (systemd-tmpfiles, a cleanup job). os.WriteFile does not recreate missing
	// parents, so without this a deleted directory would freeze metrics forever.
	if err := ensureOutputDir(); err != nil {
		return err
	}

	metrics, err := e.getMetrics(ctx)
	if err != nil {
		// If the context was cancelled we are shutting down mid-collection; don't
		// write a teardown snapshot, just unwind (the run loop will observe the
		// cancellation and return).
		if ctx.Err() != nil {
			return nil
		}
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
	if err := os.WriteFile(tmpPath, data, outputFilePermission); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		// Best-effort cleanup so a failed rename does not leave an orphaned
		// temp file behind on every collection tick.
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename %s to %s: %w", tmpPath, outputPath, err)
	}

	logger.Info("metrics written", logger.Fields{"path": outputPath, "gpuCount": len(output.GPUs)})
	return nil
}
