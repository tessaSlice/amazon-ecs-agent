//go:build unit && linux

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

package stats

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	mock_dockerapi "github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi/mocks"
	"github.com/aws/amazon-ecs-agent/agent/gpu"
	mock_resolver "github.com/aws/amazon-ecs-agent/agent/stats/resolver/mock"
	"github.com/aws/amazon-ecs-agent/ecs-agent/csiclient"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/docker/docker/api/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGPUTestEngine builds a stats engine with GPU support enabled, reading its
// metrics from filePath. publishMetrics only runs the GPU path when
// GPUSupportEnabled is set, so these tests need a config with it on; a per-test
// config copy is used so the shared package-level cfg (GPU off) is not mutated.
func newGPUTestEngine(t *testing.T, filePath string, telemetryMessages chan ecstcs.TelemetryMessage, healthMessages chan ecstcs.HealthMessage) *DockerStatsEngine {
	t.Helper()
	gpuCfg := cfg
	gpuCfg.GPUSupportEnabled = true
	engine := NewDockerStatsEngine(&gpuCfg, nil, nil, telemetryMessages, healthMessages, nil)
	var cancel context.CancelFunc
	engine.ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	engine.dcgmMetricsReader = gpu.NewDCGMMetricsReader(filePath)
	return engine
}

// gpuMetricsEmitted drains one telemetry message (an idle engine always sends
// one per publishMetrics tick) and reports whether it carried instance-level GPU
// metrics. Emission is the observable outcome of a fresh per-tick snapshot, so
// tests assert on it rather than on the engine's internal fields.
func gpuMetricsEmitted(t *testing.T, ch <-chan ecstcs.TelemetryMessage) bool {
	t.Helper()
	msg := drainOneTelemetry(t, ch)
	return msg.InstanceMetrics != nil && len(msg.InstanceMetrics.GeneralMetricsPayload) > 0
}

// TestGPUMetricsNotReEmittedForSameTimestamp verifies the de-dup cursor: a second
// read of the SAME (unchanged) metrics file does not re-emit, because its
// timestamp is not strictly newer than the last emitted one. This is de-dup, not
// wall-clock staleness (there is no staleness gate; a dead dcgm-init is handled
// implicitly because its frozen timestamp stops advancing). The engine here is
// idle (no tasks), so emission is instance-level only and the relevant cursor is
// lastInstanceGPUTimestamp; the container cursor stays empty (no container metrics).
func TestGPUMetricsNotReEmittedForSameTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write initial GPU metrics file
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 85.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)

	// Simulate 3 ticks to trigger GPU emission (gpuMetricsPublishCount >= 3)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	// First emission should carry instance GPU metrics (new timestamp) and
	// advance the instance de-dup cursor once the message is sent. The container
	// cursor stays empty because an idle send carries no per-container metrics.
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "First tick with new timestamp should emit GPU metrics")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp)
	assert.Equal(t, "", engine.lastGPUTimestamp, "container cursor must not advance on an idle send")

	// Simulate 3 more ticks with the SAME file (timestamp unchanged)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	// Second emission should NOT carry GPU metrics (same timestamp, de-duped)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Second tick with same timestamp should not emit GPU metrics")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp, "Timestamp should not change")
}

func TestGPUMetricsEmittedWhenTimestampChanges(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write initial GPU metrics
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 50.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)

	// First emission (idle engine -> instance-level emission, instance cursor advances)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages))
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp)

	// Same timestamp — no emission
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages))

	// Update file with new timestamp (simulates dcgm-init writing next tick)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:01:00Z", 99.0)

	// New timestamp — should emit again
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "New timestamp should trigger emission")
	assert.Equal(t, "2026-01-01T00:01:00Z", engine.lastInstanceGPUTimestamp)
}

func TestGPUMetricsNotEmittedBeforeThirdTick(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 75.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)

	// First tick (count goes to 1) — should not emit
	engine.gpuMetricsPublishCount = 0
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Should not emit on first tick")

	// Second tick (count goes to 2) — should not emit
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages), "Should not emit on second tick")

	// Third tick (count goes to 3, resets to 0) — should emit
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages), "Should emit on third tick")
}

func TestGPUMetricsNotEmittedWhenTimestampGoesBackward(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	// Write metrics with a recent timestamp
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:05:00Z", 90.0)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)

	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)

	// First emission succeeds (new timestamp). Idle engine -> instance-level cursor.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages))
	assert.Equal(t, "2026-01-01T00:05:00Z", engine.lastInstanceGPUTimestamp)

	// Write metrics with an OLDER timestamp (clock skew, file corruption, etc.)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:03:00Z", 10.0)

	// Should NOT emit because timestamp is older than lastInstanceGPUTimestamp
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should not emit metrics when file timestamp is older than last emitted timestamp")
	assert.Equal(t, "2026-01-01T00:05:00Z", engine.lastInstanceGPUTimestamp,
		"instance cursor should remain at the newer value")

	// Write metrics with the same timestamp as the last emitted (equal, not greater)
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:05:00Z", 50.0)

	// Should NOT emit because timestamp is equal (not strictly greater)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.False(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should not emit metrics when file timestamp equals last emitted timestamp")

	// Write metrics with a newer timestamp — should emit
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:06:00Z", 100.0)

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assert.True(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should emit metrics when file timestamp is strictly newer")
	assert.Equal(t, "2026-01-01T00:06:00Z", engine.lastInstanceGPUTimestamp)
}

// connection_lost=true means dcgm-init lost its DCGM connection (outside the
// grace period), so its sample values are not trustworthy. The engine must skip
// emission rather than publish untrustworthy telemetry, even for a fresh, newer
// timestamp that would otherwise pass the de-dup cursor.
func TestGPUMetricsNotEmittedWhenConnectionLost(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	writeGPUMetricsFileWithConnectionLost(t, filePath, "2026-01-01T00:00:00Z", 85.0, true)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	assert.False(t, gpuMetricsEmitted(t, telemetryMessages),
		"Should not emit GPU metrics when connection_lost is true")
	// The engine here is idle, so the cursor that WOULD advance if the
	// connection_lost gate were ignored is the instance cursor (the idle path
	// never ships container metrics, so lastGPUTimestamp is "" regardless and
	// asserting on it would not detect a broken gate). Assert the instance cursor
	// stayed empty: the sample was never emitted, so nothing must have advanced.
	assert.Equal(t, "", engine.lastInstanceGPUTimestamp,
		"instance de-dup cursor must not advance for a sample that was never emitted")
	assert.Equal(t, "", engine.lastGPUTimestamp,
		"container de-dup cursor must not advance (no container metrics on the idle path)")
}

// TestInstanceGPUMetricsEmittedWhenNoTaskMetrics covers finding #1: a non-idle
// instance with GPU metrics but no collectable task metrics this tick (container
// present but its stats queue is empty, so getInstanceMetrics returns
// EmptyMetricsError) must still publish a telemetry message carrying
// instance-level GPU metrics, rather than dropping them.
func TestInstanceGPUMetricsEmittedWhenNoTaskMetrics(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	const gpuUUID = "GPU-assigned-001"
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", gpuUUID)

	// The container is tracked (non-idle) but its stats queue is left empty, so
	// taskContainerMetricsUnsafe yields no metrics -> getInstanceMetrics returns
	// EmptyMetricsError -> instance-only send path.
	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, gpuUUID)

	engine.gpuMetricsPublishCount = 2 // next tick fetches
	engine.publishMetrics(false)

	msg := drainOneTelemetry(t, telemetryMessages)
	require.NotNil(t, msg.InstanceMetrics, "instance GPU metrics must be emitted even when there are no task metrics this tick")
	require.NotEmpty(t, msg.InstanceMetrics.GeneralMetricsPayload)
	// The payload values must be correct, not just present: one GPU in the snapshot
	// (limit=1) assigned to the tracked container (usage=1).
	limit, usage, ok := extractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	require.True(t, ok, "instance payload must carry both InstanceGPULimit and InstanceGPUUsageTotal")
	assert.Equal(t, int64(1), limit, "InstanceGPULimit = number of GPUs in the snapshot")
	assert.Equal(t, int64(1), usage, "InstanceGPUUsageTotal = assigned GPUs present in the snapshot")
	require.Empty(t, msg.TaskMetrics, "instance-only message must carry no task metrics")
	require.NotNil(t, msg.Metadata)
	// Marked idle so the client's idle branch forwards it (see instanceMetricsMetadata).
	assert.True(t, aws.ToBool(msg.Metadata.Idle), "instance-only message must be marked idle to route through the client idle branch")
	// Fin is left nil by instanceMetricsMetadata (the client sets it), which uniquely
	// distinguishes this synthesized instance-only path from a genuinely idle engine,
	// whose getInstanceMetrics sets Fin=true.
	assert.Nil(t, msg.Metadata.Fin, "instance-only metadata must leave Fin unset (unlike the genuine-idle path)")

	// The CONTAINER cursor must NOT advance on the instance-only path: the
	// per-container GPU metrics for this sample were not emitted, so a later
	// task-metrics-present tick must still be able to emit them.
	assert.Equal(t, "", engine.lastGPUTimestamp,
		"container cursor must not advance when only instance-level GPU metrics were sent")
	// The INSTANCE cursor DOES advance: the instance payload was sent, so a re-read
	// of the same (possibly frozen) sample must not re-publish it.
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp,
		"instance cursor must advance once the instance payload is sent")
}

// TestContainerGPUMetricsShipAfterInstanceOnlyTick is the regression test for the
// shared-cursor bug: after an instance-only publish (non-idle, no task metrics,
// fresh GPU sample), a later tick where the same container now produces stats must
// still emit that sample's CONTAINER-level GPU metrics. If the cursor were advanced
// by the instance-only send, this container emission would be silently skipped.
func TestContainerGPUMetricsShipAfterInstanceOnlyTick(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	const gpuUUID = "GPU-assigned-001"
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", gpuUUID)

	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, gpuUUID)

	// Tick 1: container has no stats yet -> EmptyMetricsError -> instance-only send.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg1 := drainOneTelemetry(t, telemetryMessages)
	require.NotNil(t, msg1.InstanceMetrics, "tick 1 should emit instance GPU metrics")
	require.Empty(t, msg1.TaskMetrics, "tick 1 is the instance-only path")
	// Container cursor stays put; instance cursor advanced (guards the frozen-file regression).
	require.Equal(t, "", engine.lastGPUTimestamp, "container cursor must not advance on the instance-only tick")
	require.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp, "instance cursor advances on the instance-only tick")

	// Now the container produces stats; the metrics FILE is unchanged (same sample).
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}

	// Tick 2: task metrics now present. Because the CONTAINER cursor was NOT advanced
	// on tick 1, the same sample is still container-fresh and its container-level GPU
	// metrics must ship — even though the instance cursor already covers this sample.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg2 := drainOneTelemetry(t, telemetryMessages)
	require.NotEmpty(t, msg2.TaskMetrics, "tick 2 should carry task metrics")
	var containerGPUFound bool
	for _, tm := range msg2.TaskMetrics {
		for _, cm := range tm.ContainerMetrics {
			if len(cm.GeneralMetricsPayload) > 0 {
				containerGPUFound = true
			}
		}
	}
	assert.True(t, containerGPUFound,
		"container-level GPU metrics for the sample must ship on the later task-metrics-present tick")
	// Now the container cursor has advanced (full emission sent). This assertion is
	// discriminating: if tick 1 had wrongly advanced the container cursor (the
	// original single-cursor bug), tick 2 would have found the sample stale, shipped
	// no container GPU metrics, and containerGPUFound above would be false.
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp,
		"container cursor advances only once the full (container-level) emission is sent")
	// The instance cursor was NOT re-committed on tick 2 for the same sample: tick 2
	// was not instance-fresh, so no instance payload rode along.
	require.Nil(t, msg2.InstanceMetrics,
		"tick 2 must not re-send instance metrics for a sample already emitted at instance level")
}

// TestContainerGPUMetricsNotReEmittedForSameTimestamp verifies container-level
// de-dup: after a full send commits the container cursor, a re-read of the SAME
// (unchanged) sample must ship task metrics WITHOUT GPU payloads and no
// instance metrics — the stale sample is not re-emitted at either level.
func TestContainerGPUMetricsNotReEmittedForSameTimestamp(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	const gpuUUID = "GPU-assigned-001"
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", gpuUUID)

	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, gpuUUID)
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}

	// Tick 1: full send — container GPU payload ships, both cursors advance.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg1 := drainOneTelemetry(t, telemetryMessages)
	require.NotEmpty(t, msg1.TaskMetrics)
	require.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp,
		"container cursor advances on the full send")
	require.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp,
		"instance cursor advances on the full send")

	// Refill stats (tick 1's send reset the queues) and tick again on the SAME file.
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg2 := drainOneTelemetry(t, telemetryMessages)
	require.NotEmpty(t, msg2.TaskMetrics, "CPU/memory task metrics still ship")
	for _, tm := range msg2.TaskMetrics {
		for _, cm := range tm.ContainerMetrics {
			assert.Empty(t, cm.GeneralMetricsPayload,
				"stale sample must not re-attach container GPU metrics")
		}
	}
	assert.Nil(t, msg2.InstanceMetrics, "stale sample must not re-emit instance metrics")
}

// TestInstanceGPUMetricsNotReEmittedForFrozenSampleOnInstanceOnlyPath is the
// regression test for the frozen-dcgm bug: on a SUSTAINED instance-only path
// (non-idle instance whose container never produces stats), a second fetch that
// re-reads the SAME (frozen) dcgm sample must NOT re-publish its instance metrics.
// The instance-level cursor, committed on the first instance-only send, suppresses
// the re-emission — without it a dead/hung dcgm-init would flood CloudWatch with
// the same stale values every ~60s.
func TestInstanceGPUMetricsNotReEmittedForFrozenSampleOnInstanceOnlyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	const gpuUUID = "GPU-assigned-001"
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", gpuUUID)

	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, gpuUUID)

	// Fetch 1: instance-only send of the fresh sample.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg1 := drainOneTelemetry(t, telemetryMessages)
	require.NotNil(t, msg1.InstanceMetrics, "fetch 1 should emit instance GPU metrics")

	// Fetch 2: the metrics FILE is unchanged (dcgm-init frozen/hung at the same
	// timestamp). Still an instance-only path (container has no stats). The sample is
	// no longer instance-fresh, so no instance payload is built; with no task metrics
	// and no instance metrics, publishMetrics sends NOTHING this tick. That absence
	// is the fix: the frozen sample is not re-published as instance metrics every
	// cycle. (Before the two-cursor fix, the instance cursor never advanced on the
	// instance-only path, so this fetch would re-emit the identical stale payload.)
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	assertNoTelemetry(t, telemetryMessages)
}

// TestContainerGPUMetricsShipAfterIdleTick is the regression test for the
// idle-path container-cursor bug: an idle send (no tasks) carries no per-container
// GPU metrics, so it must NOT advance the container cursor. If it did, a task
// appearing while the same dcgm sample is still in the file would have its
// container-level GPU metrics silently skipped.
func TestContainerGPUMetricsShipAfterIdleTick(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	const gpuUUID = "GPU-assigned-001"
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", gpuUUID)

	// Build the engine with a tracked task, then remove it so the FIRST tick is idle.
	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, gpuUUID)
	engine.removeAll()
	require.True(t, engine.isIdle(), "engine should be idle after removeAll")

	// Idle tick: instance metrics emit (idle instances still report GPU inventory),
	// but no container metrics ship, so the container cursor must stay empty.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msgIdle := drainOneTelemetry(t, telemetryMessages)
	require.NotNil(t, msgIdle.InstanceMetrics, "idle tick should still emit instance GPU metrics")
	require.Empty(t, msgIdle.TaskMetrics, "idle tick carries no task metrics")
	require.Equal(t, "", engine.lastGPUTimestamp,
		"container cursor must not advance on an idle send (no container metrics shipped)")
	require.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp,
		"instance cursor advances on the idle send")

	// A GPU task now appears (same dcgm sample still in the file) and produces stats.
	engine.addAndStartStatsContainer("c1")
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}

	// Next tick: because the idle tick did NOT consume the container cursor, the
	// same sample is still container-fresh and its container-level GPU metrics ship.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	msg := drainOneTelemetry(t, telemetryMessages)
	require.NotEmpty(t, msg.TaskMetrics, "tick should carry task metrics")
	var containerGPUFound bool
	for _, tm := range msg.TaskMetrics {
		for _, cm := range tm.ContainerMetrics {
			if len(cm.GeneralMetricsPayload) > 0 {
				containerGPUFound = true
			}
		}
	}
	assert.True(t, containerGPUFound,
		"container-level GPU metrics must ship for a task that appears after an idle tick on the same sample")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastGPUTimestamp,
		"container cursor advances only once container metrics actually ship")
}

// TestContainerCursorNotAdvancedWhenTaskShipsNoGPUPayload is the regression test
// for the loose-proxy bug: a tick that ships task metrics but NO per-container GPU
// payload (a container whose assigned GPU UUID is absent from the snapshot, or a
// non-GPU task) must NOT advance the container cursor. The commit is gated on
// containerGPUShipped, not on len(taskMetrics) > 0. If it advanced on task
// metrics alone, a GPU container that starts emitting its payload one tick later
// on the SAME dcgm sample would find it stale and drop its container-level GPU
// metrics.
func TestContainerCursorNotAdvancedWhenTaskShipsNoGPUPayload(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	// The snapshot reports GPU-in-snap; the tracked container is assigned a
	// DIFFERENT GPU, so gpuMetricsForContainer returns nothing and the shipped
	// TaskMetric carries no GeneralMetricsPayload.
	overwriteGPUMetricsFileUUID(t, filePath, "2026-01-01T00:00:00Z", "GPU-in-snap")

	engine, telemetryMessages := newGPUEngineWithTask(t, ctrl, filePath, "GPU-assigned-but-absent-from-snap")
	// Give the container CPU/memory stats so it produces a TaskMetric (without a
	// GPU payload, since its GPU is not in the snapshot).
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	msg := drainOneTelemetry(t, telemetryMessages)
	require.NotEmpty(t, msg.TaskMetrics, "the tick must carry task metrics (CPU/memory)")
	for _, tm := range msg.TaskMetrics {
		for _, cm := range tm.ContainerMetrics {
			require.Empty(t, cm.GeneralMetricsPayload,
				"no container GPU payload: the assigned GPU is not in the snapshot")
		}
	}
	// The container cursor must stay empty: no per-container GPU metrics shipped, so
	// the sample is still container-fresh for a later tick. (The instance cursor DID
	// advance because the instance payload rode along with the task metrics.)
	assert.Equal(t, "", engine.lastGPUTimestamp,
		"container cursor must NOT advance on a tick that shipped task metrics but no GPU payload")
	assert.Equal(t, "2026-01-01T00:00:00Z", engine.lastInstanceGPUTimestamp,
		"instance cursor advances because the instance payload was sent with the task metrics")
}

// assertNoTelemetry fails if any telemetry message is queued on the channel.
func assertNoTelemetry(t *testing.T, ch <-chan ecstcs.TelemetryMessage) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("expected no telemetry message, but one was published: instanceMetrics=%v taskMetrics=%d",
			msg.InstanceMetrics != nil, len(msg.TaskMetrics))
	default:
	}
}

// drainOneTelemetry returns the single telemetry message an engine tick publishes,
// failing if none is queued.
func drainOneTelemetry(t *testing.T, ch <-chan ecstcs.TelemetryMessage) ecstcs.TelemetryMessage {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	default:
		t.Fatal("expected a telemetry message to be published")
		return ecstcs.TelemetryMessage{}
	}
}

// overwriteGPUMetricsFileUUID rewrites the metrics file with a single GPU whose
// UUID is set explicitly (writeGPUMetricsFile hardcodes a fixed UUID).
func overwriteGPUMetricsFileUUID(t *testing.T, path, timestamp, uuid string) {
	t.Helper()
	data := gputypes.GPUMetricsFileData{
		Timestamp: timestamp,
		GPUs:      []gputypes.GPUMetric{{GPUUUID: uuid, GPUUtilization: aws.Float64(85.0)}},
	}
	b, err := json.MarshalIndent(data, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0644))
}

// newGPUEngineWithTask builds a GPU-enabled stats engine wired with a mock
// resolver that reports a single task "t1" whose container is assigned gpuUUID,
// reading GPU metrics from filePath. It returns the engine and its telemetry
// channel. A container "c1" is tracked (making the instance non-idle) but its
// stats queue is left empty by the caller as needed to exercise the
// EmptyMetricsError / instance-only path.
func newGPUEngineWithTask(t *testing.T, ctrl *gomock.Controller, filePath, gpuUUID string) (*DockerStatsEngine, chan ecstcs.TelemetryMessage) {
	t.Helper()
	networkMode := "bridge"
	task := &apitask.Task{Arn: "t1", Family: "f1", NetworkMode: networkMode,
		Containers: []*apicontainer.Container{{Name: "gpuContainer", GPUIDs: []string{gpuUUID}}}}
	resolver := mock_resolver.NewMockContainerMetadataResolver(ctrl)
	resolver.EXPECT().ResolveTask(gomock.Any()).AnyTimes().Return(task, nil)
	resolver.EXPECT().ResolveTaskByARN("t1").AnyTimes().Return(task, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().Return(&apicontainer.DockerContainer{
		Container: &apicontainer.Container{Name: "gpuContainer", NetworkModeUnsafe: networkMode, GPUIDs: []string{gpuUUID}},
	}, nil)

	mockDockerClient := mock_dockerapi.NewMockDockerClient(ctrl)
	mockStatsChannel := make(chan *types.StatsJSON)
	t.Cleanup(func() { close(mockStatsChannel) })
	mockDockerClient.EXPECT().Stats(gomock.Any(), gomock.Any(), gomock.Any()).Return(mockStatsChannel, nil).AnyTimes()

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := newGPUTestEngine(t, filePath, telemetryMessages, healthMessages)
	engine.resolver = resolver
	engine.client = mockDockerClient
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.csiClient = csiclient.NewDummyCSIClient()
	t.Cleanup(engine.removeAll)

	engine.addAndStartStatsContainer("c1")
	return engine, telemetryMessages
}

// TestContainerGPUMetricsEmittedForAssignedGPUs exercises the container-level GPU
// emission path in taskContainerMetricsUnsafe: a container assigned a GPU whose
// UUID is present in the per-tick snapshot must get that GPU's telemetry attached
// as ContainerMetric.GeneralMetricsPayload, and a nil snapshot must attach none.
// This path is otherwise unexercised by the idle-engine emission tests above.
func TestContainerGPUMetricsEmittedForAssignedGPUs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const gpuUUID = "GPU-assigned-001"
	networkMode := "bridge"
	task := &apitask.Task{Arn: "t1", Family: "f1", NetworkMode: networkMode}

	resolver := mock_resolver.NewMockContainerMetadataResolver(ctrl)
	resolver.EXPECT().ResolveTask("c1").AnyTimes().Return(task, nil)
	resolver.EXPECT().ResolveTaskByARN("t1").AnyTimes().Return(task, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().Return(&apicontainer.DockerContainer{
		Container: &apicontainer.Container{
			Name:              "gpuContainer",
			NetworkModeUnsafe: networkMode,
			GPUIDs:            []string{gpuUUID},
		},
	}, nil)

	mockDockerClient := mock_dockerapi.NewMockDockerClient(ctrl)
	mockStatsChannel := make(chan *types.StatsJSON)
	defer close(mockStatsChannel)
	mockDockerClient.EXPECT().Stats(gomock.Any(), gomock.Any(), gomock.Any()).Return(mockStatsChannel, nil).AnyTimes()

	engine := NewDockerStatsEngine(&cfg, nil, eventStream("TestContainerGPUMetrics"), nil, nil, nil)
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.client = mockDockerClient
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.csiClient = csiclient.NewDummyCSIClient()
	defer engine.removeAll()

	engine.addAndStartStatsContainer("c1")
	// Fill the stats queue so CPU/memory stats are present (else the container is
	// skipped before the GPU block).
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for _, fakeContainerStats := range createFakeContainerStats() {
				statsContainer.statsQueue.add(fakeContainerStats)
			}
		}
	}

	gpuSnapshot := []gputypes.GPUMetric{
		{GPUUUID: gpuUUID, GPUUtilization: aws.Float64(77.0)},
		{GPUUUID: "GPU-unassigned-999", GPUUtilization: aws.Float64(5.0)},
	}

	engine.lock.Lock()
	withGPU, gpuShipped, err := engine.taskContainerMetricsUnsafe("t1", gpuSnapshot)
	engine.lock.Unlock()
	require.NoError(t, err)
	assert.True(t, gpuShipped, "gpuShipped must be true when a GPU payload is attached")
	require.Len(t, withGPU, 1, "expected one container metric")
	require.NotEmpty(t, withGPU[0].GeneralMetricsPayload,
		"container assigned a snapshot GPU must carry GPU metrics")
	// The container is assigned exactly one GPU, so exactly one wrapper, dimensioned
	// by the assigned UUID (never the unassigned one).
	require.Len(t, withGPU[0].GeneralMetricsPayload, 1)
	require.Len(t, withGPU[0].GeneralMetricsPayload[0].Dimensions, 1)
	assert.Equal(t, gpuUUID, aws.ToString(withGPU[0].GeneralMetricsPayload[0].Dimensions[0].Value))

	// A nil snapshot must attach no container GPU metrics.
	engine.lock.Lock()
	withoutGPU, gpuShipped, err := engine.taskContainerMetricsUnsafe("t1", nil)
	engine.lock.Unlock()
	require.NoError(t, err)
	assert.False(t, gpuShipped, "gpuShipped must be false for a nil snapshot")
	require.Len(t, withoutGPU, 1)
	assert.Empty(t, withoutGPU[0].GeneralMetricsPayload,
		"nil GPU snapshot must attach no container GPU metrics")
}

// TestComputeGPUUsageTotal verifies usageTotal counts the unique GPU device IDs
// assigned across all running tasks, deduplicating a GPU shared by two tasks.
func TestComputeGPUUsageTotal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// t1 has two GPUs; t2 shares gpuB with t1 and adds gpuC. Unique = 3.
	const gpuA = "GPU-A"
	const gpuB = "GPU-B"
	const gpuC = "GPU-C"
	t1 := &apitask.Task{Arn: "t1", Containers: []*apicontainer.Container{
		{Name: "c1", GPUIDs: []string{gpuA, gpuB}},
	}}
	t2 := &apitask.Task{Arn: "t2", Containers: []*apicontainer.Container{
		{Name: "c2", GPUIDs: []string{gpuB, gpuC}},
	}}

	resolver := mock_resolver.NewMockContainerMetadataResolver(ctrl)
	resolver.EXPECT().ResolveTaskByARN("t1").AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveTaskByARN("t2").AnyTimes().Return(t2, nil)

	engine := NewDockerStatsEngine(&cfg, nil, nil, nil, nil, nil)
	engine.resolver = resolver
	// Register both tasks so computeGPUUsageTotalUnsafe iterates them.
	engine.tasksToContainers = map[string]map[string]*StatsContainer{
		"t1": {},
		"t2": {},
	}

	engine.lock.Lock()
	usage := engine.computeGPUUsageTotalUnsafe()
	engine.lock.Unlock()

	assert.Equal(t, int64(3), usage,
		"usage counts unique assigned GPU IDs; the GPU shared by both tasks is counted once")
}

// extractInstanceGPUPayloadValues parses InstanceGPULimit and
// InstanceGPUUsageTotal back out of an instance payload. Keep in sync with
// ecs-agent/gpu's ExtractInstanceGPUPayloadValues (linux-only, not vendored
// into agent/, hence this local copy).
func extractInstanceGPUPayloadValues(payload []*ecstcs.GeneralMetricsWrapper) (limit int64, usage int64, ok bool) {
	if len(payload) == 0 || payload[0] == nil {
		return 0, 0, false
	}
	var gotLimit, gotUsage bool
	for _, gm := range payload[0].GeneralMetrics {
		if gm == nil || gm.MetricName == nil || gm.MetricValueLong == nil {
			continue
		}
		switch *gm.MetricName {
		case gpuMetricNameInstanceGPULimitCount:
			limit, gotLimit = *gm.MetricValueLong, true
		case gpuMetricNameInstanceGPUUsageTotal:
			usage, gotUsage = *gm.MetricValueLong, true
		}
	}
	return limit, usage, gotLimit && gotUsage
}

func writeGPUMetricsFile(t *testing.T, path string, timestamp string, utilization float64) {
	t.Helper()
	writeGPUMetricsFileWithConnectionLost(t, path, timestamp, utilization, false)
}

// TestInstanceMetricsMetadataMarkedIdle pins the contract the instance-only GPU
// publish relies on: the synthesized metadata is marked idle (so the TCS client's
// idle branch, which dereferences *metadata.Idle, forwards it) and carries the
// cluster/instance identity and a message id the request needs.
func TestInstanceMetricsMetadataMarkedIdle(t *testing.T) {
	engine := &DockerStatsEngine{cluster: "test-cluster", containerInstanceArn: "test-ci-arn"}

	md := engine.instanceMetricsMetadata()

	require.NotNil(t, md)
	assert.True(t, aws.ToBool(md.Idle), "instance-only metadata must be idle to route through the client idle branch")
	assert.Equal(t, "test-cluster", aws.ToString(md.Cluster))
	assert.Equal(t, "test-ci-arn", aws.ToString(md.ContainerInstance))
	require.NotNil(t, md.MessageId)
	assert.NotEmpty(t, aws.ToString(md.MessageId))
}

func writeGPUMetricsFileWithConnectionLost(t *testing.T, path string, timestamp string, utilization float64, connectionLost bool) {
	t.Helper()
	data := gputypes.GPUMetricsFileData{
		Timestamp:      timestamp,
		ConnectionLost: connectionLost,
		GPUs: []gputypes.GPUMetric{
			{
				GPUUUID:        "GPU-test-001",
				GPUUtilization: aws.Float64(utilization),
			},
		},
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	require.NoError(t, err)
	err = os.WriteFile(path, bytes, 0644)
	require.NoError(t, err)
}
