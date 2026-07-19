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

//go:generate mockgen -destination=mock/$GOFILE -copyright_file=../../scripts/copyright_file github.com/aws/amazon-ecs-agent/agent/stats Engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/data"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger/field"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils"

	"github.com/cihub/seelog"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi"
	ecsengine "github.com/aws/amazon-ecs-agent/agent/engine"
	"github.com/aws/amazon-ecs-agent/agent/gpu"
	"github.com/aws/amazon-ecs-agent/agent/stats/resolver"
	taskresourcevolume "github.com/aws/amazon-ecs-agent/agent/taskresource/volume"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/csiclient"
	"github.com/aws/amazon-ecs-agent/ecs-agent/eventstream"
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/stats"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/docker/docker/api/types"
)

const (
	containerChangeHandler = "DockerStatsEngineDockerEventsHandler"
	queueResetThreshold    = 2 * dockerclient.StatsInactivityTimeout
	hostNetworkMode        = "host"
	noneNetworkMode        = "none"
	// defaultPublishServiceConnectTicker is every 3rd time service connect metrics will be sent to the backend
	// Task metrics are published at 20s interval, thus task's service metrics will be published 60s.
	defaultPublishServiceConnectTicker = 3
	// gpuMetricsPublishTickInterval fetches GPU metrics every 3rd publish tick.
	// Task metrics publish every 20s (config.DefaultContainerMetricsPublishInterval),
	// so GPU metrics are emitted roughly every 60s.
	gpuMetricsPublishTickInterval = 3
)

var (
	// EmptyMetricsError indicates an error for a task when there are no container
	// metrics to report
	EmptyMetricsError = errors.New("stats engine: no task metrics to report")
	// EmptyHealthMetricsError indicates an error for a task when there are no container
	// health metrics to report
	EmptyHealthMetricsError = errors.New("stats engine: no task health metrics to report")
)

// DockerContainerMetadataResolver implements ContainerMetadataResolver for
// DockerTaskEngine.
type DockerContainerMetadataResolver struct {
	dockerTaskEngine *ecsengine.DockerTaskEngine
}

// Engine defines methods to be implemented by the engine struct. It is
// defined to make testing easier.
type Engine interface {
	GetInstanceMetrics(includeServiceConnectStats bool) (*ecstcs.MetricsMetadata, []*ecstcs.TaskMetric, error)
	ContainerDockerStats(taskARN string, containerID string) (*types.StatsJSON, *stats.NetworkStatsPerSec, error)
	GetTaskHealthMetrics() (*ecstcs.HealthMetadata, []*ecstcs.TaskHealth, error)
	GetPublishServiceConnectTickerInterval() int32
	SetPublishServiceConnectTickerInterval(int32)
	GetPublishMetricsTicker() *time.Ticker
}

// DockerStatsEngine is used to monitor docker container events and to report
// utilization metrics of the same.

type DockerStatsEngine struct {
	ctx                        context.Context
	stopEngine                 context.CancelFunc
	client                     dockerapi.DockerClient
	cluster                    string
	containerInstanceArn       string
	lock                       sync.RWMutex
	config                     *config.Config
	containerChangeEventStream *eventstream.EventStream
	resolver                   resolver.ContainerMetadataResolver
	// tasksToContainers maps task arns to a map of container ids to StatsContainer objects.
	tasksToContainers map[string]map[string]*StatsContainer
	// tasksToHealthCheckContainers map task arns to the containers that has health check enabled
	tasksToHealthCheckContainers map[string]map[string]*StatsContainer
	// tasksToDefinitions maps task arns to task definition name and family metadata objects.
	tasksToDefinitions                  map[string]*taskDefinition
	taskToTaskStats                     map[string]*StatsTask
	taskToServiceConnectStats           map[string]*ServiceConnectStats
	publishServiceConnectTickerInterval int32
	publishMetricsTicker                *time.Ticker
	// channels to send metrics to TACS Client
	metricsChannel chan<- ecstcs.TelemetryMessage
	healthChannel  chan<- ecstcs.HealthMessage

	csiClient  csiclient.CSIClient
	dataClient data.Client

	// dcgmMetricsReader reads GPU metrics from the shared file written by dcgm-init.
	dcgmMetricsReader *gpu.DCGMMetricsReader
	// gpuMetricsPublishCount tracks ticks to emit GPU metrics every 60s (3 ticks at 20s).
	gpuMetricsPublishCount int
	// lastGPUTimestamp is the container-level GPU de-dup cursor: the timestamp of the
	// last sample whose per-container GPU metrics were actually emitted to TACS.
	// Advanced only when a tick actually shipped a per-container GPU payload (a
	// container-fresh snapshot AND at least one container carried GPU metrics), so a
	// send that carried no container GPU metrics (an idle send, an instance-only
	// send, or a tick with only non-GPU tasks) does not consume a sample the
	// container path still needs.
	lastGPUTimestamp string
	// lastInstanceGPUTimestamp is the instance-level GPU de-dup cursor: the timestamp of
	// the last sample whose instance-level GPU metrics were emitted. Advanced on every
	// send that carried an instance payload (including instance-only sends), so a frozen
	// dcgm-init file is not re-published as instance metrics every cycle. Between
	// sends, lastGPUTimestamp <= lastInstanceGPUTimestamp (the container cursor
	// does not run ahead of the instance cursor; the two commits are separate lock
	// acquisitions, so mid-send the ordering may transiently reverse — harmless,
	// the sample is then simply judged not container-fresh).
	lastInstanceGPUTimestamp string
}

// ResolveTask resolves the api task object, given container id.
func (resolver *DockerContainerMetadataResolver) ResolveTask(dockerID string) (*apitask.Task, error) {
	if resolver.dockerTaskEngine == nil {
		return nil, fmt.Errorf("Docker task engine uninitialized")
	}
	task, found := resolver.dockerTaskEngine.State().TaskByID(dockerID)
	if !found {
		return nil, fmt.Errorf("Could not map docker id to task: %s", dockerID)
	}

	return task, nil
}

func (resolver *DockerContainerMetadataResolver) ResolveTaskByARN(taskArn string) (*apitask.Task, error) {
	if resolver.dockerTaskEngine == nil {
		return nil, fmt.Errorf("docker task engine uninitialized")
	}
	task, found := resolver.dockerTaskEngine.State().TaskByArn(taskArn)
	if !found {
		return nil, fmt.Errorf("could not map task arn to task: %s", taskArn)
	}
	return task, nil

}

// ResolveContainer resolves the api container object, given container id.
func (resolver *DockerContainerMetadataResolver) ResolveContainer(dockerID string) (*apicontainer.DockerContainer, error) {
	if resolver.dockerTaskEngine == nil {
		return nil, fmt.Errorf("Docker task engine uninitialized")
	}
	container, found := resolver.dockerTaskEngine.State().ContainerByID(dockerID)
	if !found {
		return nil, fmt.Errorf("Could not map docker id to container: %s", dockerID)
	}

	return container, nil
}

// NewDockerStatsEngine creates a new instance of the DockerStatsEngine object.
// MustInit() must be called to initialize the fields of the new event listener.
func NewDockerStatsEngine(cfg *config.Config, client dockerapi.DockerClient, containerChangeEventStream *eventstream.EventStream, metricsChannel chan<- ecstcs.TelemetryMessage, healthChannel chan<- ecstcs.HealthMessage, dataClient data.Client) *DockerStatsEngine {
	return &DockerStatsEngine{
		client:                              client,
		resolver:                            nil,
		config:                              cfg,
		tasksToContainers:                   make(map[string]map[string]*StatsContainer),
		tasksToHealthCheckContainers:        make(map[string]map[string]*StatsContainer),
		tasksToDefinitions:                  make(map[string]*taskDefinition),
		taskToTaskStats:                     make(map[string]*StatsTask),
		taskToServiceConnectStats:           make(map[string]*ServiceConnectStats),
		containerChangeEventStream:          containerChangeEventStream,
		publishServiceConnectTickerInterval: 0,
		metricsChannel:                      metricsChannel,
		healthChannel:                       healthChannel,
		dataClient:                          dataClient,
		dcgmMetricsReader:                   gpu.NewDCGMMetricsReader(""),
	}
}

// synchronizeState goes through all the containers on the instance to synchronize the state on agent start
func (engine *DockerStatsEngine) synchronizeState() error {
	listContainersResponse := engine.client.ListContainers(engine.ctx, false, dockerclient.ListContainersTimeout)
	if listContainersResponse.Error != nil {
		return listContainersResponse.Error
	}

	for _, containerID := range listContainersResponse.DockerIDs {
		engine.addAndStartStatsContainer(containerID)
	}

	return nil
}

// addAndStartStatsContainer add the container into stats engine and start collecting the container stats
func (engine *DockerStatsEngine) addAndStartStatsContainer(containerID string) {
	engine.lock.Lock()
	defer engine.lock.Unlock()
	statsContainer, statsTaskContainer, err := engine.addContainerUnsafe(containerID)
	if err != nil {
		logger.Debug("Adding container to stats watchlist failed", logger.Fields{
			field.Container: containerID,
			field.Error:     err,
		})
		return
	}

	if engine.config.DisableMetrics.Enabled() || statsContainer == nil {
		return
	}

	statsContainer.StartStatsCollection()

	task, err := engine.resolver.ResolveTask(containerID)
	if err != nil {
		return
	}

	dockerContainer, errResolveContainer := engine.resolver.ResolveContainer(containerID)
	if errResolveContainer != nil {
		logger.Debug("Could not map container ID to container", logger.Fields{
			field.Container: containerID,
			field.Error:     err,
		})
		return
	}

	if task.IsNetworkModeAWSVPC() {
		// Start stats collector only for pause container
		if statsTaskContainer != nil && dockerContainer.Container.Type == apicontainer.ContainerCNIPause {
			statsTaskContainer.StartStatsCollection()
		} else {
			logger.Debug("Stats task container is nil, cannot start task stats collection", logger.Fields{
				field.Container: containerID,
			})
		}
	}

}

// MustInit initializes fields of the DockerStatsEngine object.
func (engine *DockerStatsEngine) MustInit(ctx context.Context, taskEngine ecsengine.TaskEngine, cluster string, containerInstanceArn string) error {
	derivedCtx, cancel := context.WithCancel(ctx)
	engine.stopEngine = cancel

	engine.ctx = derivedCtx
	// TODO ensure that this is done only once per engine object
	logger.Info("Initializing stats engine")
	engine.cluster = cluster
	engine.containerInstanceArn = containerInstanceArn
	engine.publishMetricsTicker = time.NewTicker(config.DefaultContainerMetricsPublishInterval)

	var err error
	engine.resolver, err = newDockerContainerMetadataResolver(taskEngine)
	if err != nil {
		return err
	}

	// Subscribe to the container change event stream
	err = engine.containerChangeEventStream.Subscribe(containerChangeHandler, engine.handleDockerEvents)
	if err != nil {
		return fmt.Errorf("Failed to subscribe to container change event stream, err %v", err)
	}
	err = engine.synchronizeState()
	if err != nil {
		logger.Warn("Synchronize the container state failed", logger.Fields{
			field.Error: err,
		})
	}

	go engine.waitToStop()
	return nil
}

// Shutdown cleans up the resources after the stats engine.
func (engine *DockerStatsEngine) Shutdown() {
	engine.stopEngine()
	engine.Disable()
}

// Disable prevents this engine from managing any additional tasks.
func (engine *DockerStatsEngine) Disable() {
	engine.lock.Lock()
}

// waitToStop waits for the container change event stream close ans stop collection metrics
func (engine *DockerStatsEngine) waitToStop() {
	// Waiting for the event stream to close
	<-engine.containerChangeEventStream.Context().Done()
	logger.Debug("Event stream closed, stop listening to the event stream")
	engine.containerChangeEventStream.Unsubscribe(containerChangeHandler)
	engine.removeAll()
	if engine.publishMetricsTicker != nil {
		engine.publishMetricsTicker.Stop()
	}
}

// removeAll stops the periodic usage data collection for all containers
func (engine *DockerStatsEngine) removeAll() {
	engine.lock.Lock()
	defer engine.lock.Unlock()

	for task, containers := range engine.tasksToContainers {
		for _, statsContainer := range containers {
			statsContainer.StopStatsCollection()
		}
		delete(engine.tasksToContainers, task)
	}

	for task := range engine.tasksToHealthCheckContainers {
		delete(engine.tasksToContainers, task)
	}
}

func (engine *DockerStatsEngine) addToStatsTaskMapUnsafe(task *apitask.Task, dockerContainerName string,
	containerType apicontainer.ContainerType) {
	var statsTaskContainer *StatsTask
	if task.IsNetworkModeAWSVPC() && containerType == apicontainer.ContainerCNIPause {
		// Excluding the pause container
		numberOfContainers := len(task.Containers) - 1
		var taskExists bool
		statsTaskContainer, taskExists = engine.taskToTaskStats[task.Arn]
		if !taskExists {
			containerInspect, err := engine.client.InspectContainer(engine.ctx, dockerContainerName,
				dockerclient.InspectContainerTimeout)
			if err != nil {
				return
			}
			containerpid := strconv.Itoa(containerInspect.State.Pid)
			statsTaskContainer, err = newStatsTaskContainer(task.Arn, task.GetID(), containerpid, numberOfContainers,
				engine.resolver, engine.config.PollingMetricsWaitDuration, task.ENIs)
			if err != nil {
				return
			}
			engine.taskToTaskStats[task.Arn] = statsTaskContainer
		} else {
			statsTaskContainer.TaskMetadata.NumberContainers = numberOfContainers
		}
	}
}

func (engine *DockerStatsEngine) addTaskToServiceConnectStatsUnsafe(taskArn string) {
	_, taskExists := engine.taskToServiceConnectStats[taskArn]
	if !taskExists {
		serviceConnectStats, err := newServiceConnectStats()
		if err != nil {
			seelog.Errorf("Error adding task %s to the service connect stats watchlist : %v", taskArn, err)
			return
		}
		engine.taskToServiceConnectStats[taskArn] = serviceConnectStats
	}
}

// addContainerUnsafe adds a container to the map of containers being watched.
func (engine *DockerStatsEngine) addContainerUnsafe(dockerID string) (*StatsContainer, *StatsTask, error) {
	// Make sure that this container belongs to a task and that the task
	// is not terminal.
	task, err := engine.resolver.ResolveTask(dockerID)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not map container to task, ignoring container: %s", dockerID)
	}

	if len(task.Arn) == 0 || len(task.Family) == 0 {
		return nil, nil, errors.Errorf("stats add container: invalid task fields, arn: %s, familiy: %s", task.Arn, task.Family)
	}

	if task.GetKnownStatus().Terminal() {
		return nil, nil, errors.Errorf("stats add container: task is terminal, ignoring container: %s, task: %s", dockerID, task.Arn)
	}

	statsContainer, err := newStatsContainer(dockerID, engine.client, engine.resolver, engine.config, engine.dataClient)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not map docker container ID to container, ignoring container: %s", dockerID)
	}

	seelog.Debugf("Adding container to stats watch list, id: %s, task: %s", dockerID, task.Arn)
	engine.tasksToDefinitions[task.Arn] = &taskDefinition{family: task.Family, version: task.Version}

	dockerContainer, errResolveContainer := engine.resolver.ResolveContainer(dockerID)
	if errResolveContainer != nil {
		seelog.Debugf("Could not map container ID to container, container: %s, err: %s", dockerID, err)
	}

	watchStatsContainer := false
	if !engine.config.DisableMetrics.Enabled() {
		// Adding container to the map for collecting stats
		watchStatsContainer = engine.addToStatsContainerMapUnsafe(task.Arn, dockerID, statsContainer,
			engine.containerMetricsMapUnsafe)
		if errResolveContainer == nil {
			engine.addToStatsTaskMapUnsafe(task, dockerContainer.DockerName, dockerContainer.Container.Type)
		}
	}

	if errResolveContainer == nil && dockerContainer.Container.HealthStatusShouldBeReported() {
		// Track the container health status
		engine.addToStatsContainerMapUnsafe(task.Arn, dockerID, statsContainer, engine.healthCheckContainerMapUnsafe)
		seelog.Debugf("Adding container to stats health check watch list, id: %s, task: %s", dockerID, task.Arn)
	}

	if errResolveContainer == nil && task.GetServiceConnectContainer() == dockerContainer.Container {
		engine.addTaskToServiceConnectStatsUnsafe(task.Arn)
	}

	if !watchStatsContainer {
		return nil, nil, nil
	}
	return statsContainer, engine.taskToTaskStats[task.Arn], nil
}

func (engine *DockerStatsEngine) containerMetricsMapUnsafe() map[string]map[string]*StatsContainer {
	return engine.tasksToContainers
}

func (engine *DockerStatsEngine) healthCheckContainerMapUnsafe() map[string]map[string]*StatsContainer {
	return engine.tasksToHealthCheckContainers
}

// addToStatsContainerMapUnsafe adds the statscontainer into stats for tracking and returns a boolean indicates
// whether this container should be tracked for collecting metrics
func (engine *DockerStatsEngine) addToStatsContainerMapUnsafe(
	taskARN, containerID string,
	statsContainer *StatsContainer,
	statsMapToUpdate func() map[string]map[string]*StatsContainer) bool {

	taskToContainerMap := statsMapToUpdate()

	// Check if this container is already being watched.
	_, taskExists := taskToContainerMap[taskARN]
	if taskExists {
		// task arn exists in map.
		_, containerExists := taskToContainerMap[taskARN][containerID]
		if containerExists {
			// container arn exists in map.
			seelog.Debugf("Container already being watched, ignoring, id: %s", containerID)
			return false
		}
	} else {
		// Create a map for the task arn if it doesn't exist yet.
		taskToContainerMap[taskARN] = make(map[string]*StatsContainer)
	}
	taskToContainerMap[taskARN][containerID] = statsContainer

	return true
}

// StartMetricsPublish starts to collect and publish task and health metrics
func (engine *DockerStatsEngine) StartMetricsPublish() {
	if engine.publishMetricsTicker == nil {
		seelog.Debug("Skipping reporting metrics through channel. Publish ticker is uninitialized")
		return
	}

	// Publish metrics immediately after we start the loop and wait for ticks. This makes sure TACS side has correct
	// TaskCount metrics in CX account (especially for short living tasks)
	engine.publishMetrics(false)
	engine.publishHealth()

	for {
		var includeServiceConnectStats bool
		metricCounter := engine.GetPublishServiceConnectTickerInterval()
		metricCounter++
		if metricCounter == defaultPublishServiceConnectTicker {
			includeServiceConnectStats = true
			metricCounter = 0
		}
		engine.SetPublishServiceConnectTickerInterval(metricCounter)
		select {
		case <-engine.publishMetricsTicker.C:
			seelog.Debugf("publishMetricsTicker triggered. Sending telemetry messages to tcsClient through channel")
			if includeServiceConnectStats {
				seelog.Debugf("service connect metrics included")
			}
			go engine.publishMetrics(includeServiceConnectStats)
			go engine.publishHealth()
		case <-engine.ctx.Done():
			return
		}
	}
}

func (engine *DockerStatsEngine) publishMetrics(includeServiceConnectStats bool) {
	publishMetricsCtx, cancel := context.WithTimeout(engine.ctx, publishMetricsTimeout)
	defer cancel()

	// Read a per-tick GPU snapshot, only when GPU support is enabled (otherwise no
	// dcgm-init writes the file, so this would be a wasted read + log lines every
	// ~20s on non-GPU instances). gpuMetrics is non-nil only when the sample is
	// container-fresh and emittable (see snapshotGPUMetrics); gpuInstanceFresh
	// reports instance-level freshness. Cursors are advanced only after the
	// message is sent (below).
	var gpuMetrics []gputypes.GPUMetric
	var gpuTimestamp string
	var gpuInstanceFresh bool
	if engine.config.GPUSupportEnabled {
		gpuMetrics, gpuTimestamp, gpuInstanceFresh = engine.snapshotGPUMetrics()
		seelog.Debugf("GPU metrics read: gpuCount=%d, gpuTimestamp=%q, instanceFresh=%t",
			len(gpuMetrics), gpuTimestamp, gpuInstanceFresh)
	}

	metricsMetadata, taskMetrics, containerGPUShipped, metricsErr := engine.getInstanceMetrics(includeServiceConnectStats, gpuMetrics)

	// Build the instance-level GPU payload only when this sample is instance-fresh,
	// so a frozen/hung dcgm-init file (same timestamp re-read after its instance
	// metrics were already sent) is not re-published every cycle. Built from the
	// per-tick snapshot so a concurrent tick cannot nil it out from under us.
	var instanceMetrics *ecstcs.InstanceMetrics
	if gpuInstanceFresh {
		if instancePayload := engine.instanceGPUPayload(gpuMetrics); instancePayload != nil {
			instanceMetrics = &ecstcs.InstanceMetrics{
				GeneralMetricsPayload: instancePayload,
			}
			seelog.Debugf("Built instance-level GPU metrics: wrapperCount=%d", len(instancePayload))
		}
	}

	// EmptyMetricsError means the instance is not idle but had no task metrics this
	// tick (e.g. containers still within their metrics grace period). We still want
	// to publish instance-level GPU metrics in that case, so treat a metrics error
	// as fatal only when there is no instance payload to carry. On any other error,
	// there is nothing to send.
	if metricsErr != nil {
		if !errors.Is(metricsErr, EmptyMetricsError) || instanceMetrics == nil {
			seelog.Warnf("Error collecting task metrics: %v", metricsErr)
			return
		}
		// Publish instance GPU metrics with no task metrics; synthesize the
		// metadata getInstanceMetrics omits on EmptyMetricsError.
		metricsMetadata = engine.instanceMetricsMetadata()
	}

	metricsMessage := ecstcs.TelemetryMessage{
		Metadata:        metricsMetadata,
		TaskMetrics:     taskMetrics,
		InstanceMetrics: instanceMetrics,
	}

	select {
	case engine.metricsChannel <- metricsMessage:
		seelog.Debugf("sent telemetry message")
		// Advance each cursor only after its metrics were actually sent, so a
		// timed-out send is retried next cycle: the container cursor only when a
		// per-container GPU payload shipped, the instance cursor whenever an
		// instance payload shipped.
		if containerGPUShipped {
			engine.commitContainerGPUTimestamp(gpuTimestamp)
		}
		if instanceMetrics != nil {
			engine.commitInstanceGPUTimestamp(gpuTimestamp)
		}
	case <-publishMetricsCtx.Done():
		seelog.Errorf("timeout sending telemetry message, discarding metrics")
	}
}

func (engine *DockerStatsEngine) publishHealth() {
	publishHealthCtx, cancel := context.WithTimeout(engine.ctx, publishMetricsTimeout)
	defer cancel()
	healthMetadata, taskHealthMetrics, healthErr := engine.GetTaskHealthMetrics()
	if healthErr == nil {
		healthMessage := ecstcs.HealthMessage{
			Metadata:      healthMetadata,
			HealthMetrics: taskHealthMetrics,
		}
		select {
		case engine.healthChannel <- healthMessage:
			seelog.Debugf("sent health message")
		case <-publishHealthCtx.Done():
			seelog.Errorf("timeout sending health message, discarding metrics")
		}
	} else {
		seelog.Warnf("Error collecting health metrics: %v", healthErr)
	}
}

// GetInstanceMetrics gets all task metrics and instance metadata from stats engine.
func (engine *DockerStatsEngine) GetInstanceMetrics(includeServiceConnectStats bool) (*ecstcs.MetricsMetadata, []*ecstcs.TaskMetric, error) {
	// No per-tick GPU snapshot on this path; publishMetrics passes one directly.
	metadata, taskMetrics, _, err := engine.getInstanceMetrics(includeServiceConnectStats, nil)
	return metadata, taskMetrics, err
}

// instanceMetricsMetadata builds metadata for an instance-GPU-only publish (no
// task metrics), which getInstanceMetrics does not supply on EmptyMetricsError.
// Idle=true routes it through the TCS client's idle branch, which forwards
// instance metrics without task metrics; the non-idle path would drop them. Idle
// here means "no task metrics in this message", not that the instance is idle —
// but note it IS serialized onto the request sent to TACS, so the backend must
// treat idle as "no task metrics" rather than "instance has no tasks".
func (engine *DockerStatsEngine) instanceMetricsMetadata() *ecstcs.MetricsMetadata {
	return engine.newMetricsMetadata(true)
}

// newMetricsMetadata builds a MetricsMetadata stamped with this instance's
// cluster/container-instance identity, the given idle flag, and a fresh message
// id. Fin is left unset for the caller to fill in.
func (engine *DockerStatsEngine) newMetricsMetadata(idle bool) *ecstcs.MetricsMetadata {
	return &ecstcs.MetricsMetadata{
		Cluster:           aws.String(engine.cluster),
		ContainerInstance: aws.String(engine.containerInstanceArn),
		Idle:              aws.Bool(idle),
		MessageId:         aws.String(uuid.NewRandom().String()),
	}
}

// getInstanceMetrics gets all task metrics and instance metadata. gpuMetrics is
// the stable per-tick GPU snapshot (may be nil). containerGPUShipped reports
// whether any returned container metric carries a GPU payload.
func (engine *DockerStatsEngine) getInstanceMetrics(includeServiceConnectStats bool, gpuMetrics []gputypes.GPUMetric) (metadata *ecstcs.MetricsMetadata, taskMetrics []*ecstcs.TaskMetric, containerGPUShipped bool, err error) {
	idle := engine.isIdle()
	metricsMetadata := engine.newMetricsMetadata(idle)

	if idle {
		seelog.Debug("Instance is idle. No task metrics to report")
		fin := true
		metricsMetadata.Fin = &fin
		return metricsMetadata, nil, false, nil
	}

	engine.lock.Lock()
	defer engine.lock.Unlock()

	if includeServiceConnectStats {
		err := engine.getServiceConnectStats()
		if err != nil {
			seelog.Errorf("Error getting service connect metrics: %v", err)
		}
	}

	taskStatsToCollect := engine.getTaskStatsToCollect()
	for taskArn := range taskStatsToCollect {
		_, isServiceConnectTask := engine.taskToServiceConnectStats[taskArn]
		containerMetrics, gpuShipped, err := engine.taskContainerMetricsUnsafe(taskArn, gpuMetrics)
		if err != nil {
			seelog.Debugf("Error getting container metrics for task: %s, err: %v", taskArn, err)
			// skip collecting service connect related metrics, if task is not service connect enabled.
			// when task metrics and health metrics are both disabled and there is a service connect task,
			// and we should not include service connect this time, we also need to skip following execution
			// to avoid invalid metrics sent to TCS
			if !isServiceConnectTask || !includeServiceConnectStats {
				continue
			}
		}

		if len(containerMetrics) == 0 {
			seelog.Debugf("Empty containerMetrics for task, ignoring, task: %s", taskArn)
			// skip collecting service connect related metrics, if task is not service connect enabled.
			// when task metrics and health metrics are both disabled and there is a service connect task,
			// and we should not include service connect this time, we also need to skip following execution
			// to avoid invalid metrics sent to TCS
			if !isServiceConnectTask || !includeServiceConnectStats {
				continue
			}
		}

		taskDef, exists := engine.tasksToDefinitions[taskArn]
		if !exists {
			seelog.Debugf("Could not map task to definition, task: %s", taskArn)
			continue
		}
		volMetrics := engine.getEBSVolumeMetrics(taskArn)
		metricTaskArn := taskArn
		taskMetric := &ecstcs.TaskMetric{
			TaskArn:               &metricTaskArn,
			TaskDefinitionFamily:  &taskDef.family,
			TaskDefinitionVersion: &taskDef.version,
			ContainerMetrics:      containerMetrics,
			VolumeMetrics:         volMetrics,
		}

		if includeServiceConnectStats {
			if serviceConnectStats, ok := engine.taskToServiceConnectStats[taskArn]; ok {
				if !serviceConnectStats.HasStatsBeenSent() {
					taskMetric.ServiceConnectMetricsWrapper = serviceConnectStats.GetStats()
					seelog.Debugf("Adding service connect stats for task : %s", taskArn)
					serviceConnectStats.SetStatsSent(true)
				}
			}
		}
		taskMetrics = append(taskMetrics, taskMetric)
		// Count GPU payloads only for task metrics that are actually included.
		if gpuShipped {
			containerGPUShipped = true
		}
	}

	if len(taskMetrics) == 0 {
		// Not idle. Expect taskMetrics to be there.
		seelog.Debugf("Return empty metrics error")
		return nil, nil, false, EmptyMetricsError
	}

	engine.resetStatsUnsafe()
	return metricsMetadata, taskMetrics, containerGPUShipped, nil
}

// GetTaskHealthMetrics returns the container health metrics
func (engine *DockerStatsEngine) GetTaskHealthMetrics() (*ecstcs.HealthMetadata, []*ecstcs.TaskHealth, error) {
	var taskHealths []*ecstcs.TaskHealth
	metadata := &ecstcs.HealthMetadata{
		Cluster:           aws.String(engine.cluster),
		ContainerInstance: aws.String(engine.containerInstanceArn),
		MessageId:         aws.String(uuid.NewRandom().String()),
	}

	if !engine.containerHealthsToMonitor() {
		return metadata, taskHealths, nil
	}

	engine.lock.RLock()
	defer engine.lock.RUnlock()

	for taskARN := range engine.tasksToHealthCheckContainers {
		taskHealth := engine.getTaskHealthUnsafe(taskARN)
		if taskHealth == nil {
			continue
		}
		taskHealths = append(taskHealths, taskHealth)
	}

	if len(taskHealths) == 0 {
		return nil, nil, EmptyHealthMetricsError
	}

	return metadata, taskHealths, nil
}

func (engine *DockerStatsEngine) isIdle() bool {
	engine.lock.RLock()
	defer engine.lock.RUnlock()

	return len(engine.tasksToContainers) == 0 && len(engine.taskToServiceConnectStats) == 0
}

func (engine *DockerStatsEngine) containerHealthsToMonitor() bool {
	engine.lock.RLock()
	defer engine.lock.RUnlock()

	return len(engine.tasksToHealthCheckContainers) != 0
}

// stopTrackingContainerUnsafe removes the StatsContainer from stats engine and
// returns true if the container is stopped or no longer tracked in agent. Otherwise
// it does nothing and return false
func (engine *DockerStatsEngine) stopTrackingContainerUnsafe(container *StatsContainer, taskARN string) bool {
	terminal, err := container.terminal()
	if err != nil {
		// Error determining if the container is terminal. This means that the container
		// id could not be resolved to a container that is being tracked by the
		// docker task engine. If the docker task engine has already removed
		// the container from its state, there's no point in stats engine tracking the
		// container. So, clean-up anyway.
		logger.Warn("Error determining if the container is terminal, removing from stats", logger.Fields{
			field.Container: container.containerMetadata.DockerID,
			field.Error:     err,
		})
		engine.doRemoveContainerUnsafe(container, taskARN)
		return true
	}
	if terminal {
		// Container is in known terminal state. Stop collection metrics.
		logger.Info("Container is terminal, removing from stats", logger.Fields{
			field.Container: container.containerMetadata.DockerID,
		})
		engine.doRemoveContainerUnsafe(container, taskARN)
		return true
	}

	return false
}

func (engine *DockerStatsEngine) getTaskHealthUnsafe(taskARN string) *ecstcs.TaskHealth {
	// Acquire the task definition information
	taskDefinition, ok := engine.tasksToDefinitions[taskARN]
	if !ok {
		seelog.Debugf("Could not map task to definitions, task: %s", taskARN)
		return nil
	}
	// Check all the stats container for the task
	containers, ok := engine.tasksToHealthCheckContainers[taskARN]
	if !ok {
		seelog.Debugf("Could not map task to health containers, task: %s", taskARN)
		return nil
	}

	// Aggregate container health information for all the containers in the task
	var containerHealths []*ecstcs.ContainerHealth
	for _, container := range containers {
		// check if the container is stopped/untracked, and remove it from stats
		//engine if needed
		if engine.stopTrackingContainerUnsafe(container, taskARN) {
			continue
		}
		dockerContainer, err := engine.resolver.ResolveContainer(container.containerMetadata.DockerID)
		if err != nil {
			seelog.Debugf("Could not resolve the Docker ID in agent state: %s", container.containerMetadata.DockerID)
			continue
		}

		// Check if the container has health check enabled
		if !dockerContainer.Container.HealthStatusShouldBeReported() {
			continue
		}

		healthInfo := dockerContainer.Container.GetHealthStatus()
		if healthInfo.Since == nil {
			// container was started but the health status isn't ready
			healthInfo.Since = aws.Time(time.Now())
		}
		containerHealth := &ecstcs.ContainerHealth{
			ContainerName: aws.String(dockerContainer.Container.Name),
			HealthStatus:  aws.String(healthInfo.Status.BackendStatus()),
			StatusSince:   (*utils.Timestamp)(aws.Time(healthInfo.Since.UTC())),
		}
		containerHealths = append(containerHealths, containerHealth)
	}

	if len(containerHealths) == 0 {
		return nil
	}

	taskHealth := &ecstcs.TaskHealth{
		Containers:            containerHealths,
		TaskArn:               aws.String(taskARN),
		TaskDefinitionFamily:  aws.String(taskDefinition.family),
		TaskDefinitionVersion: aws.String(taskDefinition.version),
	}

	return taskHealth
}

// handleDockerEvents must be called after openEventstream; it processes each
// event that it reads from the docker event stream.
func (engine *DockerStatsEngine) handleDockerEvents(events ...interface{}) error {
	for _, event := range events {
		dockerContainerChangeEvent, ok := event.(dockerapi.DockerContainerChangeEvent)
		if !ok {
			return fmt.Errorf("Unexpected event received, expected docker container change event")
		}

		switch dockerContainerChangeEvent.Status {
		case apicontainerstatus.ContainerRunning:
			engine.addAndStartStatsContainer(dockerContainerChangeEvent.DockerID)
		case apicontainerstatus.ContainerStopped:
			engine.removeContainer(dockerContainerChangeEvent.DockerID)
		default:
			seelog.Debugf("Ignoring event for container, id: %s, status: %d",
				dockerContainerChangeEvent.DockerID, dockerContainerChangeEvent.Status)
		}
	}

	return nil
}

// removeContainer deletes the container from the map of containers being watched.
// It also stops the periodic usage data collection for the container.
func (engine *DockerStatsEngine) removeContainer(dockerID string) {
	engine.lock.Lock()
	defer engine.lock.Unlock()

	// Make sure that this container belongs to a task.
	task, err := engine.resolver.ResolveTask(dockerID)
	if err != nil {
		seelog.Debugf("Could not map container to task, ignoring, err: %v, id: %s", err, dockerID)
		return
	}

	_, taskExists := engine.tasksToContainers[task.Arn]
	if !taskExists {
		seelog.Debugf("Container not being watched, id: %s", dockerID)
		return
	}

	// task arn exists in map.
	container, containerExists := engine.tasksToContainers[task.Arn][dockerID]
	if !containerExists {
		// container arn does not exist in map.
		seelog.Debugf("Container not being watched, id: %s", dockerID)
		return
	}

	engine.doRemoveContainerUnsafe(container, task.Arn)
}

// newDockerContainerMetadataResolver returns a new instance of DockerContainerMetadataResolver.
func newDockerContainerMetadataResolver(taskEngine ecsengine.TaskEngine) (*DockerContainerMetadataResolver, error) {
	dockerTaskEngine, ok := taskEngine.(*ecsengine.DockerTaskEngine)
	if !ok {
		// Error type casting docker task engine.
		return nil, fmt.Errorf("Could not load docker task engine")
	}

	resolver := &DockerContainerMetadataResolver{
		dockerTaskEngine: dockerTaskEngine,
	}

	return resolver, nil
}

// taskContainerMetricsUnsafe gets all container metrics for a task arn.
// gpuMetrics is the stable per-tick GPU snapshot (may be nil). gpuShipped
// reports whether any returned container metric carries a GPU payload.
//
//gocyclo:ignore
func (engine *DockerStatsEngine) taskContainerMetricsUnsafe(taskArn string, gpuMetrics []gputypes.GPUMetric) (containerMetrics []*ecstcs.ContainerMetric, gpuShipped bool, err error) {
	containerMap, taskExists := engine.tasksToContainers[taskArn]
	if !taskExists {
		return nil, false, fmt.Errorf("task not found")
	}

	for _, container := range containerMap {
		dockerID := container.containerMetadata.DockerID
		// Check if the container is terminal. If it is, make sure that it is
		// cleaned up properly. We might sometimes miss events from docker task
		// engine and this helps in reconciling the state. The tcs client's
		// GetInstanceMetrics probe is used as the trigger for this.
		if engine.stopTrackingContainerUnsafe(container, taskArn) {
			continue
		}
		// age is used to determine if we should or should not expect missing metrics.
		// this is because recently-started containers would normally not have their metrics
		// queue filled yet.
		age := time.Since(container.containerMetadata.StartedAt)
		// gracePeriod is the time that containers are allowed to have missing metrics
		// without throwing/logging errors.
		gracePeriod := time.Second * 30

		// CPU and Memory are both critical, so skip the container if either of these fail.
		cpuStatsSet, err := container.statsQueue.GetCPUStatsSet()
		if err != nil {
			if age < gracePeriod {
				continue
			}
			logger.Error("Error collecting cloudwatch metrics for container", logger.Fields{
				field.Container: dockerID,
				field.Error:     err,
			})
			continue
		}
		memoryStatsSet, err := container.statsQueue.GetMemoryStatsSet()
		if err != nil {
			if age < gracePeriod {
				continue
			}
			logger.Error("Error collecting cloudwatch metrics for container", logger.Fields{
				field.Container: dockerID,
				field.Error:     err,
			})
			continue
		}

		containerMetric := &ecstcs.ContainerMetric{
			ContainerName:  &container.containerMetadata.Name,
			CpuStatsSet:    cpuStatsSet,
			MemoryStatsSet: memoryStatsSet,
		}

		storageStatsSet, err := container.statsQueue.GetStorageStatsSet()
		if err != nil && age > gracePeriod {
			logger.Warn("Error getting storage stats for container", logger.Fields{
				field.Container: dockerID,
				field.Error:     err,
			})
		} else {
			containerMetric.StorageStatsSet = storageStatsSet
		}

		restartStatsSet, err := container.statsQueue.GetRestartStatsSet()
		if err != nil && age > gracePeriod {
			// we expect to get an error here if there are no restart metrics,
			// which would be common as it just means there is no restart policy on
			// the container, so just log a debug message here.
			logger.Debug("Unable to get restart stats for container", logger.Fields{
				field.Container: dockerID,
				field.Error:     err,
			})
		} else {
			containerMetric.RestartStatsSet = restartStatsSet
		}

		// Resolve the container once per iteration; ResolveContainer is a
		// side-effect-free state lookup, and both the network-stats block below
		// and the GPU block further down need the same *DockerContainer.
		dockerContainer, containerErr := engine.resolver.ResolveContainer(dockerID)

		task, err := engine.resolver.ResolveTask(dockerID)
		if err != nil {
			logger.Warn("Task not found for container", logger.Fields{
				field.Container: dockerID,
				field.Error:     err,
			})
		} else {
			if containerErr != nil {
				logger.Warn("Could not map container ID to container, container", logger.Fields{
					field.DockerId: dockerID,
					field.Error:    containerErr,
				})
			} else {
				// send network stats for default/bridge/nat/awsvpc network modes
				if task.IsNetworkModeBridge() {
					if task.IsServiceConnectEnabled() && dockerContainer.Container.Type == apicontainer.ContainerCNIPause {
						seelog.Debug("Skip adding network stats for pause container in Service Connect enabled task")
					} else {
						networkStatsSet, err := container.statsQueue.GetNetworkStatsSet()
						if err != nil && age > gracePeriod {
							// we log the error and still continue to publish cpu, memory stats
							logger.Warn("Error getting network stats for container", logger.Fields{
								field.Container: dockerID,
								field.Error:     err,
							})
						} else {
							containerMetric.NetworkStatsSet = networkStatsSet
						}
					}
				} else if task.IsNetworkModeAWSVPC() {
					taskStatsMap, taskExistsInTaskStats := engine.taskToTaskStats[taskArn]
					if !taskExistsInTaskStats {
						return nil, false, fmt.Errorf("task not found")
					}
					// do not add network stats for pause container
					if dockerContainer.Container.Type != apicontainer.ContainerCNIPause {
						networkStats, err := taskStatsMap.StatsQueue.GetNetworkStatsSet()
						if err != nil && age > gracePeriod {
							logger.Warn("Error getting network stats for container", logger.Fields{
								field.TaskARN:   taskArn,
								field.Container: dockerContainer.DockerID,
								field.Error:     err,
							})
						} else {
							containerMetric.NetworkStatsSet = networkStats
						}
					}
				}
			}
		}
		// Attach GPU metrics for the container's assigned GPUs from the per-tick
		// snapshot. gpuMetricsForContainer returns nil for empty/unmatched IDs.
		if len(gpuMetrics) > 0 && containerErr == nil {
			if gpuPayload := gpuMetricsForContainer(gpuMetrics, dockerContainer.Container.GPUIDs); len(gpuPayload) > 0 {
				containerMetric.GeneralMetricsPayload = gpuPayload
				gpuShipped = true
			}
		}

		containerMetrics = append(containerMetrics, containerMetric)
	}
	return containerMetrics, gpuShipped, nil
}

// snapshotGPUMetrics fetches dcgm-init's metrics at most once every 3 ticks (~60s)
// and returns them with their timestamp and an instanceFresh flag.
//
// metrics is non-nil only when the sample is container-fresh (newer than the
// container cursor) AND emittable, so callers use len(metrics) > 0 instead of a
// separate flag; on a throttled tick, a missing/empty/corrupt file, a
// connection-lost sample, or one not newer than the container cursor, it
// returns (nil, "", false) — the empty timestamp means "nothing read this tick",
// never a stale cursor value.
// instanceFresh reports whether it is also newer than the instance cursor. The
// two differ after any send that carried an instance payload but no per-container
// GPU payload (instance-only, idle, or task sends with no GPU payload): a re-read
// is then still container-fresh but not instance-fresh, so container metrics ship
// while a frozen sample is not re-published as instance metrics.
//
// It does NOT advance either cursor; commits happen only after a successful send
// (see commitContainer/InstanceGPUTimestamp) so a dropped send is retried.
//
// GetGPUMetrics() runs without engine.lock (the lock guards only the 3-tick
// counter and cursor reads). Overlapping ticks (a stalled send) are benign: the
// read is idempotent and commits never regress a cursor.
func (engine *DockerStatsEngine) snapshotGPUMetrics() (metrics []gputypes.GPUMetric, timestamp string, instanceFresh bool) {
	engine.lock.Lock()
	engine.gpuMetricsPublishCount++
	shouldFetch := engine.gpuMetricsPublishCount >= gpuMetricsPublishTickInterval
	if shouldFetch {
		engine.gpuMetricsPublishCount = 0
	}
	lastContainerTimestamp := engine.lastGPUTimestamp
	lastInstanceTimestamp := engine.lastInstanceGPUTimestamp
	engine.lock.Unlock()

	if !shouldFetch {
		// Throttled by the 3-tick gate: emit no GPU metrics this tick.
		return nil, "", false
	}

	gpuResult := engine.dcgmMetricsReader.GetGPUMetrics()
	if gpuResult == nil || len(gpuResult.GPUs) == 0 {
		// No file, unreadable/corrupt file, or an empty sample: nothing to emit.
		return nil, "", false
	}

	// Skip emission when dcgm-init reports its DCGM connection lost (outside the
	// grace period): while disconnected the sample's values are not trustworthy,
	// so we would rather emit nothing than publish stale/garbage telemetry. Logged
	// at Debug because this repeats every ~60s for the duration of an outage.
	if gpuResult.ConnectionLost {
		seelog.Debug("GPU metrics not emitted: dcgm-init reports connection_lost")
		return nil, "", false
	}

	// Compare as parsed time rather than raw strings so the ordering does not
	// silently depend on dcgm-init's exact RFC3339 rendering (a switch to
	// fractional seconds or a numeric offset would break lexicographic ordering).
	// GetGPUMetrics already rejects an unparseable fresh timestamp, and the cursors
	// are only ever set from a previously-validated timestamp, so a parse error
	// here is not expected; on the off chance one occurs, isNewerGPUTimestamp
	// treats it as "not newer" and we skip emission rather than emit out of order.
	if !isNewerGPUTimestamp(gpuResult.Timestamp, lastContainerTimestamp) {
		// Not newer than the container cursor (and therefore not newer than the
		// instance cursor either): the sample was already fully emitted. This is
		// how a dead/hung dcgm-init with a frozen file stops being re-published.
		return nil, "", false
	}
	instanceFresh = isNewerGPUTimestamp(gpuResult.Timestamp, lastInstanceTimestamp)
	return gpuResult.GPUs, gpuResult.Timestamp, instanceFresh
}

// isNewerGPUTimestamp reports whether candidate is a strictly later instant than
// last, both formatted as RFC3339 by dcgm-init. An empty last (no sample emitted
// yet) makes any parseable candidate newer. If either fails to parse, it returns
// false so a malformed timestamp never advances the de-dup cursor.
func isNewerGPUTimestamp(candidate, last string) bool {
	candidateTime, err := time.Parse(time.RFC3339, candidate)
	if err != nil {
		return false
	}
	if last == "" {
		return true
	}
	lastTime, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return false
	}
	return candidateTime.After(lastTime)
}

// commitContainerGPUTimestamp advances the container-level de-dup cursor after a
// message carrying per-container GPU metrics has been sent.
func (engine *DockerStatsEngine) commitContainerGPUTimestamp(timestamp string) {
	engine.commitGPUTimestamp(&engine.lastGPUTimestamp, timestamp)
}

// commitInstanceGPUTimestamp advances the instance-level de-dup cursor after a
// telemetry message carrying this sample's instance-level GPU metrics has been
// sent (either a full send or an instance-only send). Guarded against regressing
// the cursor.
func (engine *DockerStatsEngine) commitInstanceGPUTimestamp(timestamp string) {
	engine.commitGPUTimestamp(&engine.lastInstanceGPUTimestamp, timestamp)
}

// commitGPUTimestamp sets *cursor to timestamp under engine.lock, but only when
// timestamp is strictly newer, so an out-of-order or repeated commit never
// regresses a de-dup cursor.
func (engine *DockerStatsEngine) commitGPUTimestamp(cursor *string, timestamp string) {
	engine.lock.Lock()
	defer engine.lock.Unlock()
	if isNewerGPUTimestamp(timestamp, *cursor) {
		*cursor = timestamp
	}
}

// instanceGPUPayload returns the instance-level GPU telemetry payload built from
// the given per-tick GPU metrics snapshot, or nil if there are none. It reads
// engine.tasksToContainers under engine.lock, but uses the caller-provided
// snapshot for the GPU metrics so it stays consistent with the container-level
// emission from the same tick and cannot be nilled by a concurrent
// publishMetrics tick.
//
// The payload carries no wrapper dimensions: the TACS backend stamps the
// instance-scoping dimensions itself, and attaching them here made its
// dimension-set filter drop the wrapper on direct EC2 launches (no
// CapacityProviderName), so the metrics never reached CloudWatch.
func (engine *DockerStatsEngine) instanceGPUPayload(gpuMetrics []gputypes.GPUMetric) []*ecstcs.GeneralMetricsWrapper {
	if len(gpuMetrics) == 0 {
		return nil
	}
	engine.lock.Lock()
	defer engine.lock.Unlock()

	usageTotal := engine.computeGPUUsageTotalUnsafe()
	seelog.Debugf("Building instance GPU payload: gpuCount=%d, usageTotal=%d",
		len(gpuMetrics), usageTotal)
	return gpuMetricsToInstancePayload(gpuMetrics, usageTotal)
}

// computeGPUUsageTotalUnsafe counts the unique GPU device IDs assigned to running
// task containers on this instance. Callers must hold engine.lock because it
// iterates engine.tasksToContainers.
func (engine *DockerStatsEngine) computeGPUUsageTotalUnsafe() int64 {
	gpuSet := make(map[string]struct{})
	for taskArn := range engine.tasksToContainers {
		task, err := engine.resolver.ResolveTaskByARN(taskArn)
		if err != nil {
			continue
		}
		for _, container := range task.Containers {
			for _, gpuID := range container.GPUIDs {
				gpuSet[gpuID] = struct{}{}
			}
		}
	}
	return int64(len(gpuSet))
}

// GPU metric names and units as they appear in the TACS GeneralMetric payload.
// Keep in sync with ecs-agent/gpu/gpu_metrics_conversion.go (and the Two
// package's agent/actor/gpu_metrics_conversion.go): that package is linux-only
// (no !linux stub) and not vendored into agent/, so the conversion is
// duplicated here.
const (
	gpuMetricNameGPUUtilization        = "GPUUtilization"
	gpuMetricNameGPUMemoryUtilization  = "GPUMemoryUtilization"
	gpuMetricNameGPUMemoryTotal        = "GPUMemoryTotal"
	gpuMetricNameGPUMemoryUsed         = "GPUMemoryUsed"
	gpuMetricNameGPUPowerDraw          = "GPUPowerDraw"
	gpuMetricNameGPUTemperature        = "GPUTemperature"
	gpuMetricNameInstanceGPULimitCount = "InstanceGPULimit"
	gpuMetricNameInstanceGPUUsageTotal = "InstanceGPUUsageTotal"
	gpuMetricNameGPURestartAppXidCount = "GPURestartAppXidCount"

	gpuMetricUnitPercent = "Percent"
	gpuMetricUnitBytes   = "Bytes"
	gpuMetricUnitNone    = "None"
	gpuMetricUnitCount   = "Count"

	// gpuDeviceDimensionKey identifies the GPU device on per-container wrappers.
	gpuDeviceDimensionKey = "AcceleratedDevice"
)

// gpuMetricToGeneralMetricsWrapper converts one GPUMetric to a wrapper
// dimensioned by its UUID. Only non-nil metric fields are included, plus the
// XID count. Returns nil if all metric fields are nil.
func gpuMetricToGeneralMetricsWrapper(m gputypes.GPUMetric) *ecstcs.GeneralMetricsWrapper {
	var generalMetrics []*ecstcs.GeneralMetric

	if m.GPUUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        aws.String(gpuMetricNameGPUUtilization),
			MetricValueDouble: m.GPUUtilization,
			Unit:              aws.String(gpuMetricUnitPercent),
		})
	}
	if m.MemoryUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        aws.String(gpuMetricNameGPUMemoryUtilization),
			MetricValueDouble: m.MemoryUtilization,
			Unit:              aws.String(gpuMetricUnitPercent),
		})
	}
	if m.MemoryTotal != nil {
		v := int64(*m.MemoryTotal) //nolint:gosec // Max GPU memory (~141 GB) is far below int64 max.
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      aws.String(gpuMetricNameGPUMemoryTotal),
			MetricValueLong: &v,
			Unit:            aws.String(gpuMetricUnitBytes),
		})
	}
	if m.MemoryUsed != nil {
		v := int64(*m.MemoryUsed) //nolint:gosec // Max GPU memory (~141 GB) is far below int64 max.
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      aws.String(gpuMetricNameGPUMemoryUsed),
			MetricValueLong: &v,
			Unit:            aws.String(gpuMetricUnitBytes),
		})
	}
	if m.PowerDraw != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        aws.String(gpuMetricNameGPUPowerDraw),
			MetricValueDouble: m.PowerDraw,
			Unit:              aws.String(gpuMetricUnitNone),
		})
	}
	if m.Temperature != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        aws.String(gpuMetricNameGPUTemperature),
			MetricValueDouble: m.Temperature,
			Unit:              aws.String(gpuMetricUnitNone),
		})
	}
	if len(generalMetrics) == 0 {
		return nil
	}

	// Always emit the RESTART_APP XID count so customers see 0, not "No Data".
	xidCount := m.RestartAppXidCount
	generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
		MetricName:      aws.String(gpuMetricNameGPURestartAppXidCount),
		MetricValueLong: &xidCount,
		Unit:            aws.String(gpuMetricUnitCount),
	})

	return &ecstcs.GeneralMetricsWrapper{
		Dimensions: []*ecstcs.Dimension{
			{
				Key:   aws.String(gpuDeviceDimensionKey),
				Value: aws.String(m.GPUUUID),
			},
		},
		GeneralMetrics: generalMetrics,
	}
}

// gpuMetricsForContainer returns wrappers for the GPUs in metrics whose UUID is
// in gpuDeviceIDs. Returns nil when either input is empty.
func gpuMetricsForContainer(metrics []gputypes.GPUMetric, gpuDeviceIDs []string) []*ecstcs.GeneralMetricsWrapper {
	if len(metrics) == 0 || len(gpuDeviceIDs) == 0 {
		return nil
	}

	deviceIDSet := make(map[string]struct{}, len(gpuDeviceIDs))
	for _, id := range gpuDeviceIDs {
		deviceIDSet[id] = struct{}{}
	}

	var result []*ecstcs.GeneralMetricsWrapper
	for _, m := range metrics {
		if _, ok := deviceIDSet[m.GPUUUID]; !ok {
			continue
		}
		if wrapper := gpuMetricToGeneralMetricsWrapper(m); wrapper != nil {
			result = append(result, wrapper)
		}
	}
	return result
}

// gpuMetricsToInstancePayload builds the dimensionless instance-level wrapper
// carrying InstanceGPULimit (GPUs in the snapshot) and InstanceGPUUsageTotal.
// The backend stamps the instance-scoping dimensions itself.
func gpuMetricsToInstancePayload(metrics []gputypes.GPUMetric, usageTotal int64) []*ecstcs.GeneralMetricsWrapper {
	if len(metrics) == 0 {
		return nil
	}

	limitCount := int64(len(metrics))
	return []*ecstcs.GeneralMetricsWrapper{
		{
			GeneralMetrics: []*ecstcs.GeneralMetric{
				{
					MetricName:      aws.String(gpuMetricNameInstanceGPULimitCount),
					MetricValueLong: &limitCount,
					Unit:            aws.String(gpuMetricUnitCount),
				},
				{
					MetricName:      aws.String(gpuMetricNameInstanceGPUUsageTotal),
					MetricValueLong: &usageTotal,
					Unit:            aws.String(gpuMetricUnitCount),
				},
			},
		},
	}
}

func (engine *DockerStatsEngine) doRemoveContainerUnsafe(container *StatsContainer, taskArn string) {
	container.StopStatsCollection()
	dockerID := container.containerMetadata.DockerID
	delete(engine.tasksToContainers[taskArn], dockerID)
	seelog.Debugf("Deleted container from tasks, id: %s", dockerID)

	if len(engine.tasksToContainers[taskArn]) == 0 {
		// No containers in task, delete task arn from map.
		delete(engine.tasksToContainers, taskArn)
		// No need to verify if the key exists in tasksToDefinitions.
		// Delete will do nothing if the specified key doesn't exist.
		delete(engine.tasksToDefinitions, taskArn)
		seelog.Debugf("Deleted task from tasks, arn: %s", taskArn)

		// Stop collecting service connect stats for this task
		delete(engine.taskToServiceConnectStats, taskArn)
		seelog.Debugf("Deleted task from the Service Connect stats watchlist, task ARN: %s", taskArn)
	}

	// Remove the container from health container watch list
	if _, ok := engine.tasksToHealthCheckContainers[taskArn][dockerID]; !ok {
		return
	}

	delete(engine.tasksToHealthCheckContainers[taskArn], dockerID)
	if len(engine.tasksToHealthCheckContainers[taskArn]) == 0 {
		delete(engine.tasksToHealthCheckContainers, taskArn)
		seelog.Debugf("Deleted task from container health watch list, arn: %s", taskArn)
	}
}

// resetStatsUnsafe resets stats for all watched containers.
func (engine *DockerStatsEngine) resetStatsUnsafe() {
	for _, containerMap := range engine.tasksToContainers {
		for _, container := range containerMap {
			container.statsQueue.Reset()
		}
	}
}

// ContainerDockerStats returns the last stored raw docker stats object for a container
func (engine *DockerStatsEngine) ContainerDockerStats(taskARN string, containerID string) (*types.StatsJSON, *stats.NetworkStatsPerSec, error) {
	engine.lock.RLock()
	defer engine.lock.RUnlock()

	containerIDToStatsContainer, ok := engine.tasksToContainers[taskARN]
	taskToTaskStats := engine.taskToTaskStats
	if !ok {
		return nil, nil, errors.Errorf("stats engine: task '%s' for container '%s' not found",
			taskARN, containerID)
	}

	container, ok := containerIDToStatsContainer[containerID]
	if !ok {
		return nil, nil, errors.Errorf("stats engine: container not found: %s", containerID)
	}
	containerStats := container.statsQueue.GetLastStat()
	containerNetworkRateStats := container.statsQueue.GetLastNetworkStatPerSec()

	// Insert network stats in container stats
	task, err := engine.resolver.ResolveTaskByARN(taskARN)
	if err != nil {
		return nil, nil, errors.Errorf("stats engine: task '%s' not found",
			taskARN)
	}

	if task.IsNetworkModeAWSVPC() {
		taskStats, ok := taskToTaskStats[taskARN]
		if ok {
			if taskStats.StatsQueue.GetLastStat() != nil {
				containerStats.Networks = taskStats.StatsQueue.GetLastStat().Networks
			}
			containerNetworkRateStats = taskStats.StatsQueue.GetLastNetworkStatPerSec()
		} else {
			logger.Warn("Network stats not found for container", logger.Fields{
				field.TaskID:    task.GetID(),
				field.Container: containerID,
				field.Error:     err,
			})
		}
	}

	return containerStats, containerNetworkRateStats, nil
}

// getTaskStatsToCollect returns a map of taskArns for which task metrics needs to collected
func (engine *DockerStatsEngine) getTaskStatsToCollect() map[string]bool {
	taskStatsToCollect := make(map[string]bool)
	for taskArn := range engine.tasksToContainers {
		if _, taskArnExists := taskStatsToCollect[taskArn]; !taskArnExists {
			taskStatsToCollect[taskArn] = true
		}
	}
	for taskArn := range engine.taskToServiceConnectStats {
		if _, taskArnExists := taskStatsToCollect[taskArn]; !taskArnExists {
			taskStatsToCollect[taskArn] = true
		}
	}
	return taskStatsToCollect
}

// getServiceConnectStats invokes the workflow to retrieve all service connect
// related metrics for all service connect enabled tasks
func (engine *DockerStatsEngine) getServiceConnectStats() error {
	var wg sync.WaitGroup

	for taskArn := range engine.taskToServiceConnectStats {
		wg.Add(1)
		task, err := engine.resolver.ResolveTaskByARN(taskArn)
		if err != nil {
			return errors.Errorf("stats engine: task '%s' not found", taskArn)
		}
		// TODO [SC]: Check if task is service-connect enabled
		serviceConnectStats, ok := engine.taskToServiceConnectStats[taskArn]
		if !ok {
			return errors.Errorf("task '%s' is not registered to collect service connect metrics", taskArn)
		}
		go func() {
			serviceConnectStats.retrieveServiceConnectStats(task)
			wg.Done()
		}()
	}
	wg.Wait()
	return nil
}

func (engine *DockerStatsEngine) GetPublishServiceConnectTickerInterval() int32 {
	engine.lock.RLock()
	defer engine.lock.RUnlock()

	return engine.publishServiceConnectTickerInterval
}

func (engine *DockerStatsEngine) SetPublishServiceConnectTickerInterval(publishServiceConnectTickerInterval int32) {
	engine.lock.Lock()
	defer engine.lock.Unlock()

	engine.publishServiceConnectTickerInterval = publishServiceConnectTickerInterval
}

func (engine *DockerStatsEngine) GetPublishMetricsTicker() *time.Ticker {
	return engine.publishMetricsTicker
}

func (engine *DockerStatsEngine) getEBSVolumeMetrics(taskArn string) []*ecstcs.VolumeMetric {
	task, err := engine.resolver.ResolveTaskByARN(taskArn)
	if err != nil {
		logger.Error(fmt.Sprintf("Unable to get corresponding task from dd with task arn: %s", taskArn))
		return nil
	}

	if !task.IsEBSTaskAttachEnabled() {
		logger.Debug("Task not EBS-backed, skip gathering EBS volume metrics.", logger.Fields{
			"taskArn": taskArn,
		})
		return nil
	}

	// TODO: Remove the CSI client from the stats engine and just always have the CSI client created
	// since a new connection is created regardless and it'll make the stats engine less stateful
	if engine.csiClient == nil {
		client := csiclient.NewCSIClient(filepath.Join(csiclient.DefaultSocketHostPath, csiclient.DefaultImageName, csiclient.DefaultSocketName))
		engine.csiClient = &client
	}
	return engine.fetchEBSVolumeMetrics(task, taskArn)
}

func (engine *DockerStatsEngine) fetchEBSVolumeMetrics(task *apitask.Task, taskArn string) []*ecstcs.VolumeMetric {
	var metrics []*ecstcs.VolumeMetric
	for _, tv := range task.Volumes {
		if tv.Volume.GetType() == taskresourcevolume.EBSVolumeType {
			volumeId := tv.Volume.GetVolumeId()
			hostPath := tv.Volume.Source()
			volumeName := tv.Volume.GetVolumeName()
			metric, err := engine.getVolumeMetricsWithTimeout(volumeId, hostPath)
			if err != nil {
				logger.Error("Failed to gather metrics for EBS volume", logger.Fields{
					"VolumeId":             volumeId,
					"SourceVolumeHostPath": hostPath,
					"Error":                err,
				})
				continue
			}
			usedBytes := aws.Float64((float64)(metric.Used))
			totalBytes := aws.Float64((float64)(metric.Capacity))
			metrics = append(metrics, &ecstcs.VolumeMetric{
				VolumeId:   aws.String(volumeId),
				VolumeName: aws.String(volumeName),
				Utilized: &ecstcs.UDoubleCWStatsSet{
					Max:         usedBytes,
					Min:         usedBytes,
					SampleCount: aws.Int64(1),
					Sum:         usedBytes,
				},
				Size: &ecstcs.UDoubleCWStatsSet{
					Max:         totalBytes,
					Min:         totalBytes,
					SampleCount: aws.Int64(1),
					Sum:         totalBytes,
				},
			})
		}
	}
	return metrics
}

func (engine *DockerStatsEngine) getVolumeMetricsWithTimeout(volumeId, hostPath string) (*csiclient.Metrics, error) {
	derivedCtx, cancel := context.WithTimeout(engine.ctx, getVolumeMetricsTimeout)
	// releases resources if GetVolumeMetrics finishes before timeout
	defer cancel()
	return engine.csiClient.GetVolumeMetrics(derivedCtx, volumeId, hostPath)
}
