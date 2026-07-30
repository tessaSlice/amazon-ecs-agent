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

//go:build linux && unit
// +build linux,unit

package stats

// GPU metrics emission tests. Exercises GetPublishMetrics GPU contract:
//
//  1. Reader seam: gpuMetricsReader injected via SetGPUMetricsReader.
//  2. 3-tick cadence: includeGPUMetrics=true every 3rd tick.
//  3. Staleness suppression: unchanged Timestamp suppresses GPU; CPU/memory flow.
//  4. Instance payload: dimensionless InstanceGPULimit + InstanceGPUUsageTotal.
//  5. Container payload: per-device AcceleratedDevice wrapper; unassigned GPUs excluded.

import (
	"context"
	"fmt"
	"testing"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	"github.com/aws/amazon-ecs-agent/agent/api/serviceconnect"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/config"
	mock_dockerapi "github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi/mocks"
	mock_resolver "github.com/aws/amazon-ecs-agent/agent/stats/resolver/mock"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDCGMMetricsReader is a test double. Mutate data.Timestamp between calls
// to simulate fresh vs stale snapshots; reads tracks query count.
type fakeDCGMMetricsReader struct {
	data  *gputypes.GPUMetricsFileData
	reads int
}

func (f *fakeDCGMMetricsReader) GetGPUMetrics() *gputypes.GPUMetricsFileData {
	f.reads++
	return f.data
}

// setupGPUStatsEngine builds an engine watching container "c1" (task "t1",
// bridge mode) with the given GPU IDs. t.Cleanup handles goroutine shutdown.
func setupGPUStatsEngine(t *testing.T, mockCtrl *gomock.Controller, gpuIDs []string) (*DockerStatsEngine, context.CancelFunc) {
	t.Helper()
	return setupGPUStatsEngineWithConfig(t, mockCtrl, gpuIDs, &cfg, false)
}

// setupGPUStatsEngineWithConfig is setupGPUStatsEngine with an explicit config
// and an optional Service Connect container, so tests can vary the config the
// engine is constructed with.
func setupGPUStatsEngineWithConfig(t *testing.T, mockCtrl *gomock.Controller, gpuIDs []string,
	engineCfg *config.Config, serviceConnectEnabled bool) (*DockerStatsEngine, context.CancelFunc) {
	t.Helper()
	resolver := mock_resolver.NewMockContainerMetadataResolver(mockCtrl)
	mockDockerClient := mock_dockerapi.NewMockDockerClient(mockCtrl)
	container := &apicontainer.Container{
		Name:   "test",
		GPUIDs: gpuIDs,
	}
	t1 := &apitask.Task{Arn: "t1", Family: "f1", NetworkMode: "bridge"}
	if serviceConnectEnabled {
		// addContainerUnsafe registers the SC task by comparing
		// GetServiceConnectContainer() against the resolved container, so the
		// task must carry the same pointer the resolver returns.
		t1.Containers = []*apicontainer.Container{container}
		t1.ServiceConnectConfig = &serviceconnect.Config{ContainerName: container.Name}
	}
	resolver.EXPECT().ResolveTask("c1").AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().Return(&apicontainer.DockerContainer{
		Container: container,
	}, nil)
	mockDockerClient.EXPECT().Stats(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	resolver.EXPECT().ResolveTaskByARN(gomock.Any()).Return(t1, nil).AnyTimes()

	engine := NewDockerStatsEngine(engineCfg, nil, eventStream(t.Name()), nil, nil, nil)
	ctx, cancel := context.WithCancel(context.TODO())
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.client = mockDockerClient
	engine.addAndStartStatsContainer("c1")
	// Stops the spawned collect() goroutine, whose context the returned
	// cancel cannot reach; mock-free, so safe after mockCtrl.Finish().
	t.Cleanup(engine.removeAll)
	return engine, cancel
}

// feedFakeStats loads two CPU/memory samples into every watched container's
// queue. Call before each GetPublishMetrics (resetStatsUnsafe drains them).
func feedFakeStats(engine *DockerStatsEngine) {
	containerStats := createFakeContainerStats()
	for _, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			for i := 0; i < 2; i++ {
				statsContainer.statsQueue.add(containerStats[i])
			}
		}
	}
}

// requireInstanceGPUPayload asserts the instance-level GPU contract: exactly
// one dimensionless wrapper carrying InstanceGPULimit and
// InstanceGPUUsageTotal as MetricValueLong with unit Count.
func requireInstanceGPUPayload(t *testing.T, im *ecstcs.InstanceMetrics, wantLimit, wantUsage int64) {
	t.Helper()
	require.NotNil(t, im, "expected InstanceMetrics to be emitted")
	require.Len(t, im.GeneralMetricsPayload, 1, "expected exactly one instance GPU wrapper")
	wrapper := im.GeneralMetricsPayload[0]
	assert.Empty(t, wrapper.Dimensions, "instance GPU wrapper must be dimensionless; the backend stamps instance identity")
	require.Len(t, wrapper.GeneralMetrics, 2)

	var gotLimit, gotUsage *int64
	for _, gm := range wrapper.GeneralMetrics {
		require.NotNil(t, gm.MetricName)
		require.NotNil(t, gm.MetricValueLong, "instance GPU metrics are MetricValueLong")
		require.NotNil(t, gm.Unit)
		assert.Equal(t, "Count", *gm.Unit)
		switch *gm.MetricName {
		case "InstanceGPULimit":
			gotLimit = gm.MetricValueLong
		case "InstanceGPUUsageTotal":
			gotUsage = gm.MetricValueLong
		default:
			t.Errorf("unexpected instance GPU metric name: %s", *gm.MetricName)
		}
	}
	require.NotNil(t, gotLimit, "InstanceGPULimit not found")
	require.NotNil(t, gotUsage, "InstanceGPUUsageTotal not found")
	assert.Equal(t, wantLimit, *gotLimit, "InstanceGPULimit mismatch")
	assert.Equal(t, wantUsage, *gotUsage, "InstanceGPUUsageTotal mismatch")
}

// requireContainerGPUPayload asserts the container-level GPU contract: one
// AcceleratedDevice-dimensioned wrapper per assigned device, exactly the
// wanted devices, in reader order (wantUUIDs must match that order).
func requireContainerGPUPayload(t *testing.T, cm *ecstcs.ContainerMetric, wantUUIDs []string) {
	t.Helper()
	require.NotNil(t, cm)
	require.Len(t, cm.GeneralMetricsPayload, len(wantUUIDs),
		"expected one container GPU wrapper per assigned device")

	var gotUUIDs []string
	for _, wrapper := range cm.GeneralMetricsPayload {
		require.Len(t, wrapper.Dimensions, 1, "container GPU wrapper carries exactly one dimension")
		require.NotNil(t, wrapper.Dimensions[0].Key)
		require.NotNil(t, wrapper.Dimensions[0].Value)
		assert.Equal(t, "AcceleratedDevice", *wrapper.Dimensions[0].Key)
		gotUUIDs = append(gotUUIDs, *wrapper.Dimensions[0].Value)
		require.NotEmpty(t, wrapper.GeneralMetrics, "device wrapper must carry telemetry metrics")
		for _, gm := range wrapper.GeneralMetrics {
			require.NotNil(t, gm.MetricName)
			require.NotNil(t, gm.Unit)
			if *gm.MetricName == "GPUUtilization" {
				assert.NotNil(t, gm.MetricValueDouble, "GPUUtilization is MetricValueDouble")
				assert.Equal(t, "Percent", *gm.Unit)
			}
		}
	}
	assert.Equal(t, wantUUIDs, gotUUIDs, "container GPU wrapper devices mismatch")
}

// containerGPUPayloadDeviceIDs returns each wrapper's AcceleratedDevice
// dimension value, for explicit inclusion/exclusion assertions.
func containerGPUPayloadDeviceIDs(cm *ecstcs.ContainerMetric) []string {
	var ids []string
	for _, wrapper := range cm.GeneralMetricsPayload {
		for _, d := range wrapper.Dimensions {
			if d.Key != nil && *d.Key == "AcceleratedDevice" && d.Value != nil {
				ids = append(ids, *d.Value)
			}
		}
	}
	return ids
}

// assertNoGPUPayloads asserts no container carries a GPU payload, making no
// claim about the other metrics. Safe whether the surrounding container
// metrics are present (GPU-only suppression) or absent (metrics disabled).
func assertNoGPUPayloads(t *testing.T, taskMetrics []*ecstcs.TaskMetric) {
	t.Helper()
	for _, tm := range taskMetrics {
		for _, cm := range tm.ContainerMetrics {
			assert.Empty(t, cm.GeneralMetricsPayload,
				"container %s must carry no GPU payload", aws.ToString(cm.ContainerName))
		}
	}
}

// requireNoContainerGPUPayload asserts the shape of a suppressed (stale or
// off-cadence) tick: no GPU payload on any container, while CPU/memory
// metrics still flow — GPU suppression must not drop the container itself.
func requireNoContainerGPUPayload(t *testing.T, taskMetrics []*ecstcs.TaskMetric) {
	t.Helper()
	require.NotEmpty(t, taskMetrics)
	for _, tm := range taskMetrics {
		require.NotEmpty(t, tm.ContainerMetrics)
		for _, cm := range tm.ContainerMetrics {
			assert.NotNil(t, cm.CpuStatsSet, "CPU metrics must keep flowing when GPU is suppressed")
			assert.NotNil(t, cm.MemoryStatsSet, "memory metrics must keep flowing when GPU is suppressed")
		}
	}
	assertNoGPUPayloads(t, taskMetrics)
}

// TestGetPublishMetricsGPUPayloads pins what an emitting tick with fresh data
// puts on the wire at both scopes. Instance limit counts the reader's devices
// and usage counts unique assigned IDs (from container state, not the
// snapshot); the container payload is restricted to assigned devices that have
// a reading.
func TestGetPublishMetricsGPUPayloads(t *testing.T) {
	testCases := []struct {
		name string
		// gpuIDs are assigned to the container; snapshotGPUs are what the
		// reader reports for the host.
		gpuIDs               []string
		snapshotGPUs         []gputypes.GPUMetric
		wantInstanceLimit    int64
		wantInstanceUsage    int64
		wantContainerDevices []string // nil means no container payload at all
		wantAbsentDevices    []string
	}{
		{
			name:   "assigned subset of host GPUs emits both scopes",
			gpuIDs: []string{"GPU-1"},
			snapshotGPUs: []gputypes.GPUMetric{
				{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(50.0)},
				{GPUUUID: "GPU-2", GPUUtilization: aws.Float64(10.0)},
			},
			wantInstanceLimit:    2,
			wantInstanceUsage:    1,
			wantContainerDevices: []string{"GPU-1"},
			wantAbsentDevices:    []string{"GPU-2"},
		},
		{
			// GPU-9 is assigned but absent from the snapshot: skipped silently,
			// with no empty wrapper. It still counts toward instance usage.
			name:   "assigned device with no reading is skipped",
			gpuIDs: []string{"GPU-1", "GPU-3", "GPU-9"},
			snapshotGPUs: []gputypes.GPUMetric{
				{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(50.0)},
				{GPUUUID: "GPU-2", GPUUtilization: aws.Float64(10.0)},
				{GPUUUID: "GPU-3", GPUUtilization: aws.Float64(75.0)},
			},
			wantInstanceLimit:    3,
			wantInstanceUsage:    3,
			wantContainerDevices: []string{"GPU-1", "GPU-3"},
			wantAbsentDevices:    []string{"GPU-2", "GPU-9"},
		},
		{
			name:              "no assigned GPUs emits instance scope only",
			gpuIDs:            nil,
			snapshotGPUs:      []gputypes.GPUMetric{{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(25.0)}},
			wantInstanceLimit: 1,
			wantInstanceUsage: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			engine, cancel := setupGPUStatsEngine(t, mockCtrl, tc.gpuIDs)
			defer cancel()

			fake := &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
				Timestamp: "2026-07-19T00:00:00Z",
				Healthy:   true,
				GPUs:      tc.snapshotGPUs,
			}}
			engine.SetGPUMetricsReader(fake)

			feedFakeStats(engine)

			metadata, taskMetrics, instanceMetrics, err := engine.GetPublishMetrics(false, true)
			require.NoError(t, err)
			require.NotNil(t, metadata)

			requireInstanceGPUPayload(t, instanceMetrics, tc.wantInstanceLimit, tc.wantInstanceUsage)

			if tc.wantContainerDevices == nil {
				// No container payload, but CPU/memory must be unaffected.
				requireNoContainerGPUPayload(t, taskMetrics)
				return
			}

			require.Len(t, taskMetrics, 1)
			require.Len(t, taskMetrics[0].ContainerMetrics, 1)
			cm := taskMetrics[0].ContainerMetrics[0]
			requireContainerGPUPayload(t, cm, tc.wantContainerDevices)
			gotDeviceIDs := containerGPUPayloadDeviceIDs(cm)
			for _, absent := range tc.wantAbsentDevices {
				assert.NotContains(t, gotDeviceIDs, absent,
					"device %s is unassigned or has no reading; it must not produce a wrapper", absent)
			}
			assert.NotNil(t, cm.CpuStatsSet)
			assert.NotNil(t, cm.MemoryStatsSet)
			assert.GreaterOrEqual(t, fake.reads, 1, "engine should have queried the DCGM metrics reader")
		})
	}
}

// TestGetPublishMetricsSkipsStaleGPUMetrics: an unchanged reader Timestamp
// means stale data — suppressed at both scopes, resuming once the Timestamp
// changes. The stale tick asserts on containers too: checking only
// instanceMetrics would miss leaked container wrappers or a dropped
// container.
func TestGetPublishMetricsSkipsStaleGPUMetrics(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	// Assign the GPU so a container payload exists to be suppressed.
	engine, cancel := setupGPUStatsEngine(t, mockCtrl, []string{"GPU-1"})
	defer cancel()

	fake := &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
		Timestamp: "2026-07-19T00:00:00Z",
		Healthy:   true,
		GPUs:      []gputypes.GPUMetric{{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(25.0)}},
	}}
	engine.SetGPUMetricsReader(fake)

	// Emitting tick #1: fresh data -> emitted at both scopes.
	feedFakeStats(engine)
	_, taskMetrics, instanceMetrics, err := engine.GetPublishMetrics(false, true)
	require.NoError(t, err)
	requireInstanceGPUPayload(t, instanceMetrics, 1, 1)
	require.Len(t, taskMetrics, 1)
	require.Len(t, taskMetrics[0].ContainerMetrics, 1)
	requireContainerGPUPayload(t, taskMetrics[0].ContainerMetrics[0], []string{"GPU-1"})

	// Emitting tick #2: same timestamp -> stale. Both scopes suppressed
	// (tick #1's GPU-1 wrapper must not reappear); CPU/memory keep flowing.
	feedFakeStats(engine)
	_, taskMetrics, instanceMetrics, err = engine.GetPublishMetrics(false, true)
	require.NoError(t, err)
	assert.Nil(t, instanceMetrics, "stale GPU data (unchanged timestamp) must not be re-emitted")
	requireNoContainerGPUPayload(t, taskMetrics)

	// Emitting tick #3: new snapshot -> emission resumes at both scopes.
	fake.data.Timestamp = "2026-07-19T00:01:00Z"
	feedFakeStats(engine)
	_, taskMetrics, instanceMetrics, err = engine.GetPublishMetrics(false, true)
	require.NoError(t, err)
	requireInstanceGPUPayload(t, instanceMetrics, 1, 1)
	require.Len(t, taskMetrics, 1)
	require.Len(t, taskMetrics[0].ContainerMetrics, 1)
	requireContainerGPUPayload(t, taskMetrics[0].ContainerMetrics[0], []string{"GPU-1"})
}

// TestGPUMetricsEmittedOnlyWhenFlagSet verifies that GPU payloads are
// attached only when includeGPUMetrics is true (set by StartMetricsPublish
// every 3rd tick); CPU/memory metrics flow regardless.
func TestGPUMetricsEmittedOnlyWhenFlagSet(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	engine, cancel := setupGPUStatsEngine(t, mockCtrl, []string{"GPU-1"})
	defer cancel()

	fake := &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
		Timestamp: "2026-07-19T00:00:00Z",
		Healthy:   true,
		GPUs:      []gputypes.GPUMetric{{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(25.0)}},
	}}
	engine.SetGPUMetricsReader(fake)

	for tick := 1; tick <= 2*defaultPublishGPUMetricsTicker; tick++ {
		// Fresh data every round so staleness never interferes.
		fake.data.Timestamp = fmt.Sprintf("2026-07-19T00:00:%02dZ", tick)
		feedFakeStats(engine)

		// Simulate StartMetricsPublish: pass true on every 3rd tick.
		includeGPU := tick%defaultPublishGPUMetricsTicker == 0
		_, taskMetrics, instanceMetrics, err := engine.GetPublishMetrics(false, includeGPU)
		require.NoError(t, err, "tick %d", tick)

		if includeGPU {
			requireInstanceGPUPayload(t, instanceMetrics, 1, 1)
			require.Len(t, taskMetrics, 1, "tick %d", tick)
			require.Len(t, taskMetrics[0].ContainerMetrics, 1, "tick %d", tick)
			requireContainerGPUPayload(t, taskMetrics[0].ContainerMetrics[0], []string{"GPU-1"})
		} else {
			assert.Nil(t, instanceMetrics, "GPU metrics must not be emitted when includeGPUMetrics=false (tick %d)",
				tick)
			requireNoContainerGPUPayload(t, taskMetrics)
		}
	}
}

// TestGetPublishMetricsSuppressesGPUForUnusableSnapshot verifies that GPU
// metrics are suppressed at both scopes when the reader's snapshot is unusable
// (connection lost, nil, or no GPU entries) and emitted normally when it is
// healthy. Every subtest is an emitting tick, so the reader's snapshot is the
// only variable.
func TestGetPublishMetricsSuppressesGPUForUnusableSnapshot(t *testing.T) {
	testCases := []struct {
		name          string
		reader        gpuMetricsReader
		expectGPUEmit bool
	}{
		{
			// Also the ECS_DISABLE_METRICS path: that flag leaves the reader nil
			// at construction (see TestGPUMetricsReaderFollowsDisableMetrics), so
			// this case covers its emission behaviour at both scopes.
			name:          "no reader wired up suppresses GPU",
			reader:        nil,
			expectGPUEmit: false,
		},
		{
			name: "connection lost suppresses GPU at both scopes",
			reader: &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
				Timestamp:      "2026-07-19T00:00:00Z",
				Healthy:        true,
				ConnectionLost: true,
				GPUs:           []gputypes.GPUMetric{{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(50.0)}},
			}},
			expectGPUEmit: false,
		},
		{
			name:          "nil snapshot from reader suppresses GPU",
			reader:        &fakeDCGMMetricsReader{data: nil},
			expectGPUEmit: false,
		},
		{
			name: "snapshot with no GPU entries suppresses GPU",
			reader: &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
				Timestamp: "2026-07-19T00:00:00Z",
				Healthy:   true,
				GPUs:      nil,
			}},
			expectGPUEmit: false,
		},
		{
			name: "connection healthy emits GPU normally",
			reader: &fakeDCGMMetricsReader{data: &gputypes.GPUMetricsFileData{
				Timestamp: "2026-07-19T00:00:00Z",
				Healthy:   true,
				GPUs:      []gputypes.GPUMetric{{GPUUUID: "GPU-1", GPUUtilization: aws.Float64(50.0)}},
			}},
			expectGPUEmit: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			engine, cancel := setupGPUStatsEngine(t, mockCtrl, []string{"GPU-1"})
			defer cancel()

			engine.SetGPUMetricsReader(tc.reader)

			feedFakeStats(engine)
			_, taskMetrics, instanceMetrics, err := engine.GetPublishMetrics(false, true)
			require.NoError(t, err)

			if tc.expectGPUEmit {
				requireInstanceGPUPayload(t, instanceMetrics, 1, 1)
				require.Len(t, taskMetrics, 1)
				require.Len(t, taskMetrics[0].ContainerMetrics, 1)
				requireContainerGPUPayload(t, taskMetrics[0].ContainerMetrics[0], []string{"GPU-1"})
			} else {
				assert.Nil(t, instanceMetrics, "instance GPU must not be emitted for an unusable snapshot")
				requireNoContainerGPUPayload(t, taskMetrics)
			}

			// CPU/memory must keep flowing regardless of GPU suppression.
			require.NotEmpty(t, taskMetrics)
			require.NotEmpty(t, taskMetrics[0].ContainerMetrics)
			assert.NotNil(t, taskMetrics[0].ContainerMetrics[0].CpuStatsSet,
				"CPU metrics must keep flowing")
			assert.NotNil(t, taskMetrics[0].ContainerMetrics[0].MemoryStatsSet,
				"memory metrics must keep flowing")
		})
	}
}

// TestGPUMetricsReaderFollowsDisableMetrics pins the construction-time gate:
// the reader is wired up only when GPU support is on and ECS_DISABLE_METRICS is
// off, so the disabled path costs nothing per tick. Cases that expect no reader
// also run a publish tick end to end, confirming nothing reaches TACS at either
// scope. The Service Connect variant matters: an SC task keeps the engine
// non-idle, so GetPublishMetrics reaches the GPU block a nil reader shuts down.
func TestGPUMetricsReaderFollowsDisableMetrics(t *testing.T) {
	testCases := []struct {
		name                  string
		gpuSupportEnabled     bool
		disableMetrics        bool
		serviceConnectEnabled bool
		expectReader          bool
	}{
		{
			name:              "GPU support on and metrics enabled wires up the reader",
			gpuSupportEnabled: true,
			disableMetrics:    false,
			expectReader:      true,
		},
		{
			name:              "metrics disabled leaves the reader nil",
			gpuSupportEnabled: true,
			disableMetrics:    true,
		},
		{
			name:                  "metrics disabled leaves the reader nil with a Service Connect task",
			gpuSupportEnabled:     true,
			disableMetrics:        true,
			serviceConnectEnabled: true,
		},
		{
			name:              "GPU support off leaves the reader nil",
			gpuSupportEnabled: false,
			disableMetrics:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			engineCfg := cfg
			engineCfg.GPUSupportEnabled = tc.gpuSupportEnabled
			if tc.disableMetrics {
				engineCfg.DisableMetrics = config.BooleanDefaultFalse{Value: config.ExplicitlyEnabled}
			}

			engine, cancel := setupGPUStatsEngineWithConfig(t, mockCtrl, []string{"GPU-1"},
				&engineCfg, tc.serviceConnectEnabled)
			defer cancel()

			if tc.expectReader {
				assert.NotNil(t, engine.gpuCollector.reader)
				return
			}
			require.Nil(t, engine.gpuCollector.reader)

			// A nil reader must also produce no GPU payload on the wire. No
			// SetGPUMetricsReader here: injecting one would route around the
			// construction-time gate under test.
			feedFakeStats(engine)
			// Both flags true: the tick counters advance in lockstep, so every
			// 3rd tick sets both.
			_, taskMetrics, instanceMetrics, err := engine.GetPublishMetrics(
				tc.serviceConnectEnabled, true)
			if err != nil {
				require.ErrorIs(t, err, EmptyMetricsError)
			}
			assert.Nil(t, instanceMetrics,
				"instance GPU payload must not be emitted without a reader")
			assertNoGPUPayloads(t, taskMetrics)
		})
	}
}
