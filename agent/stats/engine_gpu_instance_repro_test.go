//go:build unit && linux

// Regression test: verifies instance-level GPU metrics survive a concurrent
// publishMetrics tick that nils engine.currentGPUMetrics between the container
// read and the instance read. Guards against reintroducing the split-lock drop
// where instanceGPUPayload re-read the shared field instead of using the
// per-tick snapshot captured in publishMetrics.

package stats

import (
	"context"
	"path/filepath"
	"testing"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/gpu"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mock_resolver "github.com/aws/amazon-ecs-agent/agent/stats/resolver/mock"
)

// Reproduces the split-lock drop: instanceGPUPayload must build from the
// per-tick snapshot captured while holding engine.lock in publishMetrics, NOT
// re-read engine.currentGPUMetrics. A concurrent publishMetrics tick nils
// engine.currentGPUMetrics on entry; if the instance path re-read that shared
// field it would observe nil and drop instance metrics while container metrics
// (read earlier in the same tick) survive.
//
// This test drives the same read sequence publishMetrics uses: capture the
// snapshot after the guarded 3rd-tick read, simulate a concurrent tick nilling
// the shared field, then confirm the snapshot still yields an instance payload.
func TestInstanceGPUPayloadSurvivesConcurrentNil(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 75.0)

	containerID := "c1"
	t1 := &apitask.Task{
		Arn: "t1", Family: "f1", Version: "1",
		KnownStatusUnsafe: apitaskstatus.TaskRunning,
		Containers:        []*apicontainer.Container{{Name: containerID, GPUIDs: []string{"GPU-test-001"}}},
	}
	resolver := mock_resolver.NewMockContainerMetadataResolver(mockCtrl)
	resolver.EXPECT().ResolveTask(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveTaskByARN(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().Return(&apicontainer.DockerContainer{
		DockerID:  containerID,
		Container: &apicontainer.Container{Name: containerID, KnownStatusUnsafe: apicontainerstatus.ContainerRunning, GPUIDs: []string{"GPU-test-001"}},
	}, nil)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("repro"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.addAndStartStatsContainer(containerID)
	cs := createFakeContainerStats()
	for _, sc := range engine.tasksToContainers["t1"] {
		for i := 0; i < 2; i++ {
			sc.statsQueue.add(cs[i])
		}
	}

	// Drive the 3rd-tick guarded read exactly as publishMetrics does, then
	// capture the per-tick snapshot while holding the lock.
	engine.lock.Lock()
	engine.gpuMetricsPublishCount = 3
	engine.gpuMetricsPublishCount = 0
	if gpuResult := engine.dcgmHandler.GetGPUMetrics(); gpuResult != nil && len(gpuResult.Metrics) > 0 && gpuResult.Timestamp > engine.lastGPUTimestamp {
		engine.lastGPUTimestamp = gpuResult.Timestamp
		engine.currentGPUMetrics = gpuResult.Metrics
	}
	gpuMetrics := engine.currentGPUMetrics
	engine.lock.Unlock()
	require.NotEmpty(t, gpuMetrics, "3rd-tick read should populate the per-tick snapshot")

	// Simulate a CONCURRENT publishMetrics tick that nils the shared field AFTER
	// the snapshot was taken (this is the window that dropped instance metrics
	// before the fix). This is exactly what refreshing on a non-3rd tick does.
	engine.lock.Lock()
	engine.currentGPUMetrics = nil
	engine.lock.Unlock()
	require.Empty(t, engine.currentGPUMetrics, "concurrent tick should have nilled the shared field")

	// The instance payload must still be built from the snapshot, not the field.
	instancePayload := engine.instanceGPUPayload(gpuMetrics)
	assert.NotNil(t, instancePayload, "instance payload must survive a concurrent nil of the shared field")
	assert.NotEmpty(t, instancePayload, "instance payload should contain InstanceGPULimit/InstanceGPUUsageTotal")
}
