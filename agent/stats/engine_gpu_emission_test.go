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
	"github.com/aws/amazon-ecs-agent/agent/gpu"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	gpuconvert "github.com/aws/amazon-ecs-agent/ecs-agent/gpu"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mock_resolver "github.com/aws/amazon-ecs-agent/agent/stats/resolver/mock"
)

// writeMultiGPUMetricsFile writes a gpu-metrics.json describing one GPU per
// (uuid, util) pair. Mirrors writeGPUMetricsFile but parameterizes the device
// list so tests can exercise multi-GPU installed counts. The timestamp MUST be
// RFC3339-parseable or GetGPUMetrics rejects the file.
func writeMultiGPUMetricsFile(t *testing.T, path string, timestamp string, uuids []string, utils []float64) {
	t.Helper()
	require.Equal(t, len(uuids), len(utils), "uuids and utils must be the same length")
	gpus := make([]gpu.GPUMetric, 0, len(uuids))
	for i, id := range uuids {
		gpus = append(gpus, gpu.GPUMetric{
			GPUUUID:        id,
			GPUUtilization: aws.Float64(utils[i]),
		})
	}
	data := gpu.GPUMetricsFileData{
		Timestamp: timestamp,
		Healthy:   true,
		GPUs:      gpus,
	}
	bytes, err := json.MarshalIndent(data, "", "  ")
	require.NoError(t, err)
	err = os.WriteFile(path, bytes, 0644)
	require.NoError(t, err)
}

// instanceMetricByName walks the (single) instance GeneralMetricsWrapper and
// returns the GeneralMetric with the given MetricName, or nil. Enables
// order-independent assertions on the instance payload.
func instanceMetricByName(im *ecstcs.InstanceMetrics, name string) *ecstcs.GeneralMetric {
	if im == nil || len(im.GeneralMetricsPayload) == 0 {
		return nil
	}
	for _, m := range im.GeneralMetricsPayload[0].GeneralMetrics {
		if aws.ToString(m.MetricName) == name {
			return m
		}
	}
	return nil
}

func TestEmission_NonIdleSingleGPU_DeliversInstanceMetricsOnChannel(t *testing.T) {
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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("single"), telemetryMessages, healthMessages, nil)
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

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages

	require.NotNil(t, msg.Metadata)
	assert.Equal(t, defaultCluster, aws.ToString(msg.Metadata.Cluster))
	assert.Equal(t, defaultContainerInstance, aws.ToString(msg.Metadata.ContainerInstance))
	assert.False(t, aws.ToBool(msg.Metadata.Idle))

	require.NotNil(t, msg.InstanceMetrics)
	require.Len(t, msg.InstanceMetrics.GeneralMetricsPayload, 1)
	wrapper := msg.InstanceMetrics.GeneralMetricsPayload[0]
	// The instance wrapper is scoped to the container instance so CloudWatch can
	// break InstanceGPU* metrics down per instance. EC2InstanceId is not set in
	// this test (SetEC2InstanceID not called), so only ContainerInstanceId appears.
	require.Len(t, wrapper.Dimensions, 1)
	assert.Equal(t, "ContainerInstanceId", aws.ToString(wrapper.Dimensions[0].Key))
	assert.Equal(t, defaultContainerInstance, aws.ToString(wrapper.Dimensions[0].Value))
	require.Len(t, wrapper.GeneralMetrics, 2)

	limit := instanceMetricByName(msg.InstanceMetrics, "InstanceGPULimit")
	require.NotNil(t, limit)
	assert.Equal(t, int64(1), aws.ToInt64(limit.MetricValueLong))
	assert.Equal(t, "Count", aws.ToString(limit.Unit))
	assert.Nil(t, limit.MetricValueDouble)

	usage := instanceMetricByName(msg.InstanceMetrics, "InstanceGPUUsageTotal")
	require.NotNil(t, usage)
	assert.Equal(t, int64(1), aws.ToInt64(usage.MetricValueLong))
	assert.Equal(t, "Count", aws.ToString(usage.Unit))

	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(1), limitV)
	assert.Equal(t, int64(1), usageV)
}

// TestEmission_InstanceDimensions_ScopedToContainerAndEC2Instance verifies that,
// once SetEC2InstanceID is called and the container instance is an ARN, the
// instance-level GPU wrapper carries both ContainerInstanceId (the short resource
// ID, not the full ARN) and EC2InstanceId dimensions so CloudWatch can break the
// InstanceGPU* metrics down per instance.
func TestEmission_InstanceDimensions_ScopedToContainerAndEC2Instance(t *testing.T) {
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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("instancedims"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	// ARN form so we can assert it is shortened to the resource ID for the dimension.
	engine.containerInstanceArn = "arn:aws:ecs:us-east-1:123456789012:container-instance/cluster/abc123def456"
	engine.SetEC2InstanceID("i-0123456789abcdef0")
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.addAndStartStatsContainer(containerID)
	cs := createFakeContainerStats()
	for _, sc := range engine.tasksToContainers["t1"] {
		for i := 0; i < 2; i++ {
			sc.statsQueue.add(cs[i])
		}
	}

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages
	require.NotNil(t, msg.InstanceMetrics)
	require.Len(t, msg.InstanceMetrics.GeneralMetricsPayload, 1)

	dims := map[string]string{}
	for _, d := range msg.InstanceMetrics.GeneralMetricsPayload[0].Dimensions {
		dims[aws.ToString(d.Key)] = aws.ToString(d.Value)
	}
	assert.Equal(t, "abc123def456", dims["ContainerInstanceId"], "ContainerInstanceId should be the short resource ID")
	assert.Equal(t, "i-0123456789abcdef0", dims["EC2InstanceId"], "EC2InstanceId should match SetEC2InstanceID")
	assert.Len(t, dims, 2, "expected exactly ContainerInstanceId + EC2InstanceId dimensions")
}

func TestEmission_MultiGPU_LimitEqualsInstalledUsageEqualsAssigned(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	writeMultiGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", []string{"GPU-0", "GPU-1"}, []float64{40, 60})

	containerID := "c1"
	t1 := &apitask.Task{
		Arn: "t1", Family: "f1", Version: "1",
		KnownStatusUnsafe: apitaskstatus.TaskRunning,
		Containers:        []*apicontainer.Container{{Name: containerID, GPUIDs: []string{"GPU-0"}}},
	}
	resolver := mock_resolver.NewMockContainerMetadataResolver(mockCtrl)
	resolver.EXPECT().ResolveTask(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveTaskByARN(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().Return(&apicontainer.DockerContainer{
		DockerID:  containerID,
		Container: &apicontainer.Container{Name: containerID, KnownStatusUnsafe: apicontainerstatus.ContainerRunning, GPUIDs: []string{"GPU-0"}},
	}, nil)

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("multigpu"), telemetryMessages, healthMessages, nil)
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

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages
	require.NotNil(t, msg.InstanceMetrics)

	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(2), limitV)
	assert.Equal(t, int64(1), usageV)
	assert.Equal(t, "Count", aws.ToString(instanceMetricByName(msg.InstanceMetrics, "InstanceGPULimit").Unit))
	assert.Equal(t, "Count", aws.ToString(instanceMetricByName(msg.InstanceMetrics, "InstanceGPUUsageTotal").Unit))
}

func TestEmission_MultiContainer_UsageTotalDedupsSharedGPU(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	writeMultiGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", []string{"GPU-0", "GPU-1"}, []float64{10, 20})

	t1 := &apitask.Task{
		Arn: "t1", Family: "f1", Version: "1",
		KnownStatusUnsafe: apitaskstatus.TaskRunning,
		Containers: []*apicontainer.Container{
			{Name: "c1", GPUIDs: []string{"GPU-0"}},
			{Name: "c2", GPUIDs: []string{"GPU-0", "GPU-1"}},
		},
	}
	resolver := mock_resolver.NewMockContainerMetadataResolver(mockCtrl)
	resolver.EXPECT().ResolveTask(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveTaskByARN(gomock.Any()).AnyTimes().Return(t1, nil)
	resolver.EXPECT().ResolveContainer(gomock.Any()).AnyTimes().DoAndReturn(
		func(dockerID string) (*apicontainer.DockerContainer, error) {
			switch dockerID {
			case "c2":
				return &apicontainer.DockerContainer{
					DockerID:  "c2",
					Container: &apicontainer.Container{Name: "c2", KnownStatusUnsafe: apicontainerstatus.ContainerRunning, GPUIDs: []string{"GPU-0", "GPU-1"}},
				}, nil
			default:
				return &apicontainer.DockerContainer{
					DockerID:  "c1",
					Container: &apicontainer.Container{Name: "c1", KnownStatusUnsafe: apicontainerstatus.ContainerRunning, GPUIDs: []string{"GPU-0"}},
				}, nil
			}
		})

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("multicontainer"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.addAndStartStatsContainer("c1")
	engine.addAndStartStatsContainer("c2")
	cs := createFakeContainerStats()
	for _, sc := range engine.tasksToContainers["t1"] {
		for i := 0; i < 2; i++ {
			sc.statsQueue.add(cs[i])
		}
	}

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages
	require.NotNil(t, msg.InstanceMetrics)

	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(2), limitV)
	assert.Equal(t, int64(2), usageV)
}

func TestEmission_InstanceAndContainerGPUMetrics_CoexistOnSameTick(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	writeGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", 88.0)

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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("coexist"), telemetryMessages, healthMessages, nil)
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

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages

	// INSTANCE SIDE
	require.NotNil(t, msg.InstanceMetrics)
	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(1), limitV)
	assert.Equal(t, int64(1), usageV)
	// Instance wrapper is scoped by ContainerInstanceId (EC2InstanceId unset here).
	instWrapper := msg.InstanceMetrics.GeneralMetricsPayload[0]
	require.Len(t, instWrapper.Dimensions, 1)
	assert.Equal(t, "ContainerInstanceId", aws.ToString(instWrapper.Dimensions[0].Key))
	assert.Equal(t, defaultContainerInstance, aws.ToString(instWrapper.Dimensions[0].Value))

	// CONTAINER SIDE (same message)
	require.Len(t, msg.TaskMetrics, 1)
	tm := msg.TaskMetrics[0]
	assert.Equal(t, "t1", aws.ToString(tm.TaskArn))
	require.Len(t, tm.ContainerMetrics, 1)
	cm := tm.ContainerMetrics[0]
	require.NotEmpty(t, cm.GeneralMetricsPayload)
	cw := cm.GeneralMetricsPayload[0]

	require.Len(t, cw.Dimensions, 1)
	assert.Equal(t, "AcceleratedDevice", aws.ToString(cw.Dimensions[0].Key))
	assert.Equal(t, "GPU-test-001", aws.ToString(cw.Dimensions[0].Value))

	var util *ecstcs.GeneralMetric
	for _, m := range cw.GeneralMetrics {
		if aws.ToString(m.MetricName) == "GPUUtilization" {
			util = m
			break
		}
	}
	require.NotNil(t, util)
	assert.Equal(t, 88.0, aws.ToFloat64(util.MetricValueDouble))
	assert.Equal(t, "Percent", aws.ToString(util.Unit))
}

func TestEmission_NoInstanceMetricsOnChannelBeforeThirdTick(t *testing.T) {
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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("beforethird"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.addAndStartStatsContainer(containerID)
	// Re-seed the stats queues with fresh (unsent) data points. Each publish
	// marks buffered stats as "sent", so a subsequent tick needs a fresh pair
	// or GetCPUStatsSet/GetMemoryStatsSet fail and the task is dropped.
	seedStats := func() {
		cs := createFakeContainerStats()
		for _, sc := range engine.tasksToContainers["t1"] {
			for i := 0; i < 2; i++ {
				sc.statsQueue.add(cs[i])
			}
		}
	}
	seedStats()

	engine.gpuMetricsPublishCount = 0

	// Tick 1
	engine.publishMetrics(false)
	require.Len(t, telemetryMessages, 1)
	msg1 := <-telemetryMessages
	assert.Nil(t, msg1.InstanceMetrics)
	require.Len(t, msg1.TaskMetrics, 1)
	require.Len(t, msg1.TaskMetrics[0].ContainerMetrics, 1)
	assert.Empty(t, msg1.TaskMetrics[0].ContainerMetrics[0].GeneralMetricsPayload)

	// Tick 2
	seedStats()
	engine.publishMetrics(false)
	require.Len(t, telemetryMessages, 1)
	msg2 := <-telemetryMessages
	assert.Nil(t, msg2.InstanceMetrics)

	// Tick 3
	seedStats()
	engine.publishMetrics(false)
	require.Len(t, telemetryMessages, 1)
	msg3 := <-telemetryMessages
	require.NotNil(t, msg3.InstanceMetrics)
	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg3.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(1), limitV)
	assert.Equal(t, int64(1), usageV)
}

func TestEmission_NoInstanceMetricsOnChannelWhenTimestampStale(t *testing.T) {
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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("stale"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.addAndStartStatsContainer(containerID)
	// Each publish marks buffered stats "sent"; re-seed before every tick so
	// the task keeps producing container metrics and is not dropped.
	seedStats := func() {
		cs := createFakeContainerStats()
		for _, sc := range engine.tasksToContainers["t1"] {
			for i := 0; i < 2; i++ {
				sc.statsQueue.add(cs[i])
			}
		}
	}
	seedStats()

	// First: fresh timestamp emits instance metrics.
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	require.Len(t, telemetryMessages, 1)
	msg1 := <-telemetryMessages
	require.NotNil(t, msg1.InstanceMetrics)

	// Second: same timestamp (stale) — no instance metrics, but task metrics remain.
	seedStats()
	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)
	require.Len(t, telemetryMessages, 1)
	msg2 := <-telemetryMessages
	assert.Nil(t, msg2.InstanceMetrics)
	assert.NotEmpty(t, msg2.TaskMetrics)
}

func TestEmission_NoInstanceMetricsOnChannelWhenGPUFileMissing(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

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
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("missing"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.resolver = resolver
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filepath.Join(t.TempDir(), "does-not-exist.json"))

	engine.addAndStartStatsContainer(containerID)
	cs := createFakeContainerStats()
	for _, sc := range engine.tasksToContainers["t1"] {
		for i := 0; i < 2; i++ {
			sc.statsQueue.add(cs[i])
		}
	}

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages
	assert.Nil(t, msg.InstanceMetrics)
	assert.NotEmpty(t, msg.TaskMetrics)
	require.Len(t, msg.TaskMetrics[0].ContainerMetrics, 1)
	assert.Empty(t, msg.TaskMetrics[0].ContainerMetrics[0].GeneralMetricsPayload)
}

func TestEmission_IdleInstanceStillEmitsInstanceGPULimit(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	writeMultiGPUMetricsFile(t, filePath, "2026-01-01T00:00:00Z", []string{"GPU-0", "GPU-1"}, []float64{5, 5})

	telemetryMessages := make(chan ecstcs.TelemetryMessage, 10)
	healthMessages := make(chan ecstcs.HealthMessage, 10)
	engine := NewDockerStatsEngine(&cfg, nil, eventStream("idle"), telemetryMessages, healthMessages, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.ctx = ctx
	engine.cluster = defaultCluster
	engine.containerInstanceArn = defaultContainerInstance
	engine.dcgmHandler = gpu.NewDCGMHandler(filePath)

	engine.gpuMetricsPublishCount = 2
	engine.publishMetrics(false)

	require.Len(t, telemetryMessages, 1)
	msg := <-telemetryMessages
	assert.True(t, aws.ToBool(msg.Metadata.Idle))
	assert.Empty(t, msg.TaskMetrics)
	require.NotNil(t, msg.InstanceMetrics)

	limitV, usageV, ok := gpuconvert.ExtractInstanceGPUPayloadValues(msg.InstanceMetrics.GeneralMetricsPayload)
	assert.True(t, ok)
	assert.Equal(t, int64(2), limitV)
	assert.Equal(t, int64(0), usageV)
}
