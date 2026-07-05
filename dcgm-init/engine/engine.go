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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/gpu/dcgm"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
)

const (
	// DefaultOutputPath is the shared file where dcgm-init writes GPU metrics
	// for the agent to consume. It must match the path the agent reads from.
	DefaultOutputPath = "/var/run/ecs/gpu-metrics.json"

	// DefaultCollectionFreq is the default interval between metrics collections.
	// It is also used as a fallback when a non-positive interval is configured,
	// since time.NewTicker panics on a non-positive duration.
	DefaultCollectionFreq = 60 * time.Second

	// DefaultPidFilePath is where the "start" process records its PID so that a
	// separate "stop" process can locate and signal it. It lives alongside the
	// metrics file under /var/run/ecs.
	DefaultPidFilePath = "/var/run/ecs/dcgm-init.pid"

	// outputDirPermission is the permission for the directory holding the metrics file.
	outputDirPermission = 0755

	// outputFilePermission is the permission for the metrics file (and its staging copy).
	outputFilePermission = 0644

	// pidFilePermission is the permission for the pid file.
	pidFilePermission = 0644
)

// Exit codes for the dcgm-init binary. They mirror ecs-init's convention so the
// dcgm-init systemd unit's RestartPreventExitStatus=5 can distinguish an
// unrecoverable (terminal) failure — which a restart cannot fix — from a
// transient failure that systemd should retry.
const (
	// TerminalFailureExitCode signals an unrecoverable failure. Because the unit
	// sets RestartPreventExitStatus=5, exiting with this code prevents systemd
	// from restarting dcgm-init in a hot loop over a problem a restart won't fix.
	TerminalFailureExitCode = 5

	// DefaultErrorExitCode signals a transient/generic failure. With
	// Restart=on-failure, systemd will restart the service after RestartSec.
	DefaultErrorExitCode = 1
)

// TerminalError marks a failure that a restart cannot fix (e.g. a bad output
// path). main() maps it to TerminalFailureExitCode. It mirrors
// ecs-init/engine.TerminalError.
type TerminalError struct {
	err error
}

func (e *TerminalError) Error() string {
	if e.err == nil {
		return "terminal error"
	}
	return e.err.Error()
}

// Unwrap lets errors.As/errors.Is see the wrapped cause.
func (e *TerminalError) Unwrap() error { return e.err }

// NewTerminalError wraps err as a *TerminalError, marking it unrecoverable so
// main() exits with TerminalFailureExitCode.
func NewTerminalError(err error) *TerminalError {
	return &TerminalError{err: err}
}

// Engine drives the dcgm-init metrics collection loop: it connects to DCGM via
// the dcgm.Client, periodically collects GPU metrics, and writes them to a shared
// JSON file that the agent reads.
type Engine struct {
	client         dcgm.Client
	outputPath     string
	collectionFreq time.Duration
	oneShot        bool
	// pidPath is the file where "start" records its PID and where "stop" looks
	// to find the running process to signal. Empty disables pid-file handling.
	pidPath string

	// mu guards cancel, which is set while Start()'s run loop is active and
	// read by Stop(). A context.CancelFunc is itself safe to call concurrently;
	// mu only protects the pointer assignment/read against a data race.
	mu     sync.Mutex
	cancel context.CancelFunc

	// signalProcess sends a signal to the given PID. Overridable in tests.
	signalProcess func(pid int, sig syscall.Signal) error
}

// New creates an Engine with the given configuration.
func New(socketPath string, outputPath string, pidPath string, collectionFreq time.Duration, oneShot bool) *Engine {
	config := dcgm.Config{
		SocketPath:                socketPath,
		InitializationGracePeriod: dcgm.DefaultInitializationGracePeriod,
	}

	// Guard against a non-positive interval: time.NewTicker panics on a
	// duration <= 0, which would crash the long-running "start" command.
	if collectionFreq <= 0 {
		logger.Warn("dcgm-init collection interval is non-positive, using default", logger.Fields{
			"configured": collectionFreq,
			"default":    DefaultCollectionFreq,
		})
		collectionFreq = DefaultCollectionFreq
	}

	return &Engine{
		client:         dcgm.NewClient(config),
		outputPath:     outputPath,
		pidPath:        pidPath,
		collectionFreq: collectionFreq,
		oneShot:        oneShot,
		signalProcess:  syscall.Kill,
	}
}

// Start connects to DCGM and runs the metrics collection loop until
// a SIGTERM/SIGINT is received. This is the main entry point for the
// "start" command.
func (e *Engine) Start() error {
	defer func() {
		if err := e.client.Shutdown(); err != nil {
			logger.Warn("dcgm-init failed to shut down DCGM client cleanly", logger.Fields{"error": err})
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Publish the cancel func so an in-process Stop() call can terminate the
	// run loop. Cleared on return so a later Stop() cannot cancel a stale
	// context. NOTE: under systemd, `start` and `stop` are separate process
	// invocations, so Stop() only reaches this cancel when Start() and Stop()
	// run in the same process (e.g. tests). See Stop() for details.
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.cancel = nil
		e.mu.Unlock()
	}()

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

	outputDir := filepath.Dir(e.outputPath)
	if err := os.MkdirAll(outputDir, outputDirPermission); err != nil {
		// A bad output path is a configuration problem a restart cannot fix;
		// mark it terminal so systemd does not restart-loop over it.
		return NewTerminalError(fmt.Errorf("failed to create output directory %s: %w", outputDir, err))
	}

	// Record our PID so a separate "dcgm-init stop" process can find and signal
	// us, and remove it on exit. Failure to write is non-fatal: collection still
	// works, only the cross-process stop shortcut is unavailable.
	if err := e.writePidFile(); err != nil {
		logger.Warn("dcgm-init failed to write pid file, cross-process stop will be unavailable", logger.Fields{
			"pidFile": e.pidPath,
			"error":   err,
		})
	} else {
		defer e.removePidFile()
	}

	return e.run(ctx)
}

// writePidFile atomically records the current process PID at e.pidPath. It is a
// no-op if no pid path is configured.
func (e *Engine) writePidFile() error {
	if e.pidPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.pidPath), outputDirPermission); err != nil {
		return fmt.Errorf("failed to create pid file directory: %w", err)
	}
	// Write to a staging file then rename so a concurrent reader never sees a
	// partially written PID.
	staging := e.pidPath + ".staging"
	data := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(staging, data, pidFilePermission); err != nil {
		return fmt.Errorf("failed to write pid file %s: %w", staging, err)
	}
	if err := os.Rename(staging, e.pidPath); err != nil {
		os.Remove(staging)
		return fmt.Errorf("failed to rename pid file to %s: %w", e.pidPath, err)
	}
	return nil
}

// removePidFile deletes the pid file written by writePidFile. It is best-effort:
// a missing file or removal error is logged at debug level and otherwise ignored.
func (e *Engine) removePidFile() {
	if e.pidPath == "" {
		return
	}
	if err := os.Remove(e.pidPath); err != nil && !os.IsNotExist(err) {
		logger.Debug("dcgm-init failed to remove pid file", logger.Fields{"pidFile": e.pidPath, "error": err})
	}
}

// Stop terminates the metrics collection loop started by Start().
//
// It handles two process models:
//
//  1. In-process (tests, or an embedded caller): if a run loop is active in
//     THIS process, Stop() cancels its context directly and returns.
//
//  2. Cross-process (the CLI / systemd case): `dcgm-init start` and
//     `dcgm-init stop` are separate process invocations, so the stop process's
//     Engine has no in-process cancel. Stop() then reads the PID recorded by
//     the running start process and sends it SIGTERM. The start process's
//     signal watcher (see Start()) catches SIGTERM and cancels its own run
//     context, which is what actually stops collection.
//
// If neither a live in-process loop nor a pid file is found, Stop() treats the
// service as already stopped and returns nil.
func (e *Engine) Stop() error {
	// Case 1: a run loop is active in this process.
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		logger.Info("dcgm-init stop requested, cancelling metrics collection in-process")
		cancel()
		return nil
	}

	// Case 2: signal the separate running process via its pid file.
	return e.stopViaPidFile()
}

// stopViaPidFile reads the PID recorded by a running "start" process and sends
// it SIGTERM so its signal watcher cancels the run context. A missing pid file
// is treated as "already stopped" (nil). A stale pid file (process gone) is
// cleaned up and also treated as not-running.
func (e *Engine) stopViaPidFile() error {
	if e.pidPath == "" {
		logger.Info("dcgm-init stop requested but no pid file configured; nothing to stop")
		return nil
	}

	pid, err := e.readPidFile()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Info("dcgm-init stop requested but no pid file found; nothing to stop", logger.Fields{"pidFile": e.pidPath})
			return nil
		}
		return fmt.Errorf("failed to read pid file %s: %w", e.pidPath, err)
	}

	logger.Info("dcgm-init stop requested, signalling running process", logger.Fields{"pid": pid, "signal": "SIGTERM"})
	if err := e.signalProcess(pid, syscall.SIGTERM); err != nil {
		// ESRCH means the recorded process no longer exists: a stale pid file
		// left behind by a crash. Clean it up and report success.
		if errors.Is(err, syscall.ESRCH) {
			logger.Info("dcgm-init process not running, removing stale pid file", logger.Fields{"pid": pid, "pidFile": e.pidPath})
			e.removePidFile()
			return nil
		}
		return fmt.Errorf("failed to signal dcgm-init process %d: %w", pid, err)
	}
	return nil
}

// readPidFile parses the PID from e.pidPath. It returns an error wrapping
// os.ErrNotExist when the file is absent.
func (e *Engine) readPidFile() (int, error) {
	data, err := os.ReadFile(e.pidPath)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("malformed pid file contents %q: %w", string(data), err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d in pid file", pid)
	}
	return pid, nil
}

// run reconciles the DCGM connection, then collects and writes metrics. In
// one-shot mode it collects once and returns; otherwise it collects on each
// tick of collectionFreq until the context is cancelled.
func (e *Engine) run(ctx context.Context) error {
	reinitialized, err := e.client.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("initial DCGM reconciliation failed: %w", err)
	}
	logger.Info("DCGM client reconciled", logger.Fields{"reinitialized": reinitialized})

	if e.oneShot {
		return e.collectAndWrite(ctx)
	}

	ticker := time.NewTicker(e.collectionFreq)
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

	stagingPath := e.outputPath + ".staging"
	if err := os.WriteFile(stagingPath, data, outputFilePermission); err != nil {
		return fmt.Errorf("failed to write metrics to %s: %w", stagingPath, err)
	}
	if err := os.Rename(stagingPath, e.outputPath); err != nil {
		// Best-effort cleanup so a failed rename does not leave an orphaned
		// staging file behind on every collection tick.
		os.Remove(stagingPath)
		return fmt.Errorf("failed to rename %s to %s: %w", stagingPath, e.outputPath, err)
	}

	logger.Info("metrics written", logger.Fields{"path": e.outputPath, "gpuCount": len(output.GPUs)})
	return nil
}
