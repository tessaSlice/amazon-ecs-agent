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
	"sync/atomic"
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

// Filesystem operations used by ensureOutputDir and collectAndWrite, indirected
// through package vars so tests can deterministically exercise each branch (a
// real MkdirAll or write failure is permission-dependent and would not reproduce
// under root, and outputPath is a fixed const that points outside a test's temp
// dir). Production code uses the real os/syscall implementations.
var (
	osStat      = os.Stat
	osMkdirAll  = os.MkdirAll
	checkAccess = syscall.Access
	osWriteFile = os.WriteFile
	osRename    = os.Rename
	osRemove    = os.Remove
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

	// collectsInFlight counts GetMetrics goroutines that have not yet returned.
	// getMetrics may abandon a goroutine (blocked in a non-cancellable DCGM cgo
	// call) when the context is cancelled; Start consults this before calling
	// client.Shutdown so it never frees DCGM C resources out from under an
	// in-flight call. It is >0 only while such a call is executing.
	collectsInFlight atomic.Int32
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
		// If a GetMetrics call is still executing (its goroutine was abandoned
		// because a non-cancellable DCGM cgo call wedged during shutdown), skip
		// Shutdown: freeing the DCGM connection and field groups now would race
		// the in-flight cgo call against a concurrent C-resource teardown. The
		// process is exiting anyway, so the OS reclaims everything; a wedged
		// nv-hostengine connection is not worth a use-after-free to close.
		if inFlight := e.collectsInFlight.Load(); inFlight > 0 {
			logger.Warn("dcgm-init skipping DCGM shutdown; a metrics collection is still in flight",
				logger.Fields{"collectsInFlight": inFlight})
			return
		}
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

	info, statErr := osStat(outputDir)
	if statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: output path %s exists but is not a directory", ErrSetup, outputDir)
		}
		// Directory exists — confirm we can write into it rather than assuming.
		if accessErr := checkAccess(outputDir, unixWOK); accessErr != nil {
			return fmt.Errorf("%w: output directory %s is not writable: %w", ErrSetup, outputDir, accessErr)
		}
		return nil
	}
	if !os.IsNotExist(statErr) {
		return fmt.Errorf("%w: failed to stat output directory %s: %w", ErrSetup, outputDir, statErr)
	}

	// Directory does not exist — create it as a fallback.
	if err := osMkdirAll(outputDir, outputDirPermission); err != nil {
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

	return e.runLoop(ctx, ticker.C)
}

// runLoop performs the initial collection and then reconciles and collects on
// each tick until the context is cancelled or a persistent failure escalates.
// The tick channel is a parameter (rather than created inside) so tests can
// drive the cadence deterministically without waiting real time; production
// passes the real collectionFreq ticker.
//
// A single failed tick is expected and non-fatal (transient DCGM hiccup, brief
// connection loss). But a run that can never make progress — a permanently
// unwritable output path, a deleted output directory, or a dead/unresponsive
// nv-hostengine past the grace period — must not loop forever silently. A
// single counter tracks consecutive ticks that failed to emit real GPU metrics,
// for ANY reason (reconcile error, collection/disconnect, or file-write error).
// Once it reaches maxConsecutiveFailures we return an error so the process exits
// non-zero and systemd restarts it; any tick that emits real metrics resets it.
//
// Tracking a single "did not make progress" counter (rather than one per failure
// mode) is deliberate: a live-but-unreadable DCGM connection makes Reconcile
// succeed while GetMetrics fails, which would slip past separate reconcile/write
// counters and wipe the file forever. What matters for liveness is simply
// whether real metrics are being emitted.
func (e *Engine) runLoop(ctx context.Context, tick <-chan time.Time) error {
	// If a shutdown signal arrived during startup (e.g. while Reconcile was
	// blocking), skip the initial collection so we don't write during teardown.
	if ctx.Err() != nil {
		logger.Info("dcgm-init is shutting down before initial metrics collection")
		return nil
	}

	consecutiveFailures := 0
	// escalate records the outcome of a tick and, on a persistent inability to
	// emit real metrics, returns a terminal error for runLoop to propagate. The
	// error deliberately does NOT wrap the underlying cause with %w: a runtime
	// escalation is a generic failure (ExitError), distinct from a startup
	// ErrSetup (ExitSetupError), and the cause may transitively wrap ErrSetup.
	escalate := func(emitted bool, cause error) error {
		if emitted {
			consecutiveFailures = 0
			return nil
		}
		consecutiveFailures++
		logger.Warn("dcgm-init failed to emit GPU metrics", logger.Fields{
			"error":               cause,
			"consecutiveFailures": consecutiveFailures,
		})
		if consecutiveFailures >= maxConsecutiveFailures {
			return fmt.Errorf("dcgm-init giving up after %d consecutive failures to emit GPU metrics: %v",
				consecutiveFailures, cause)
		}
		return nil
	}

	emitted, err := e.collectAndWrite(ctx)
	if escErr := escalate(emitted, err); escErr != nil {
		return escErr
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("dcgm-init is shutting down metrics collection")
			return nil
		case <-tick:
			// Reconcile reconnects a dropped DCGM connection; log its error but
			// let the collection outcome below drive escalation (a reconcile
			// failure is followed by a collection failure on the same tick).
			if _, err := e.client.Reconcile(ctx); err != nil {
				logger.Warn("dcgm-init client reconciliation failed", logger.Fields{"error": err})
			}
			emitted, err := e.collectAndWrite(ctx)
			if escErr := escalate(emitted, err); escErr != nil {
				return escErr
			}
		}
	}
}

// metricsOutput is the JSON structure written to the shared metrics file. Its
// shape and tags must match what the agent reads. It carries only telemetry: GPU
// health is not reported here. When metrics are unavailable (DCGM disconnected),
// the same structure is written with an empty GPUs list rather than truncating
// the file, so the reader always parses valid JSON and distinguishes "no GPU
// data" from a malformed file.
type metricsOutput struct {
	Timestamp string               `json:"timestamp"`
	GPUs      []gputypes.GPUMetric `json:"gpus"`
}

// getMetrics calls the DCGM client's GetMetrics but honors ctx cancellation.
// The client's GetMetrics performs synchronous cgo calls into nv-hostengine and
// does not itself observe ctx, so a wedged nv-hostengine could otherwise block
// the collection tick indefinitely and stall signal-driven shutdown. Running it
// in a goroutine lets us return promptly on ctx.Done(); the goroutine (and the
// blocked cgo call) is abandoned but the buffered channel ensures it does not
// leak on the normal completion path.
//
// collectsInFlight is incremented for the lifetime of the client call so that
// Start's deferred Shutdown can detect an abandoned-but-still-running call and
// avoid tearing down DCGM C resources concurrently with it.
func (e *Engine) getMetrics(ctx context.Context) ([]gputypes.GPUMetric, error) {
	type result struct {
		metrics []gputypes.GPUMetric
		err     error
	}
	resCh := make(chan result, 1)
	e.collectsInFlight.Add(1)
	go func() {
		defer e.collectsInFlight.Add(-1)
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
// to the shared output file. When metrics are available it writes a fresh
// {timestamp, gpus} snapshot and reports emitted=true; when the DCGM connection
// is down (GetMetrics fails), it writes a valid but empty {timestamp, gpus: []}
// snapshot so a reader parses cleanly and sees no stale GPU data, and reports
// emitted=false so the run loop can escalate a persistent inability to collect.
//
// The returned bool is "real GPU metrics were emitted this call"; err is only a
// marshal or write/rename failure (also emitted=false). A shutdown-triggered
// cancellation returns (false, nil) without touching the file. Either write is
// atomic: content is staged and renamed so readers never see a partial file.
func (e *Engine) collectAndWrite(ctx context.Context) (emitted bool, err error) {
	// Re-ensure the output directory on every collection: it is created once in
	// Start(), but /var/run is tmpfs and the directory can be pruned at runtime
	// (systemd-tmpfiles, a cleanup job). os.WriteFile does not recreate missing
	// parents, so without this a deleted directory would freeze metrics forever.
	if err := ensureOutputDir(); err != nil {
		return false, err
	}

	metrics, err := e.getMetrics(ctx)
	if err != nil {
		// If the context was cancelled we are shutting down mid-collection; don't
		// touch the file, just unwind (the run loop will observe the cancellation
		// and return).
		if ctx.Err() != nil {
			return false, nil
		}
		// DCGM is unavailable; write an empty (but valid-JSON) snapshot so the
		// reader parses cleanly and does not keep serving the last (now stale)
		// metrics as if they were current. Report emitted=false so a persistent
		// failure escalates rather than looping forever.
		logger.Warn("dcgm-init failed to collect GPU metrics, writing empty snapshot", logger.Fields{"error": err})
		if writeErr := e.writeSnapshot(nil); writeErr != nil {
			return false, writeErr
		}
		return false, nil
	}

	if err := e.writeSnapshot(metrics); err != nil {
		return false, err
	}

	logger.Info("metrics written", logger.Fields{"path": outputPath, "gpuCount": len(metrics)})
	return true, nil
}

// writeSnapshot marshals a {timestamp, gpus} snapshot and atomically writes it to
// the shared output file. A nil metrics slice is normalized to an empty (non-nil)
// list so the JSON is always a valid object with "gpus": [] rather than null,
// which is how a disconnection is represented.
func (e *Engine) writeSnapshot(metrics []gputypes.GPUMetric) error {
	if metrics == nil {
		metrics = []gputypes.GPUMetric{}
	}
	output := metricsOutput{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs:      metrics,
	}
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metrics: %w", err)
	}
	return e.writeOutput(data)
}

// writeOutput atomically replaces the shared metrics file with data. Content is
// written to a staging file and then renamed onto the final path so a concurrent
// reader (the agent) never observes a partially written file. The reader always
// consumes outputPath; outputPath+".tmp" is only the transient write target that
// the rename moves into place.
func (e *Engine) writeOutput(data []byte) error {
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
	return nil
}
