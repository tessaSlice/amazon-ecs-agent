//go:build unit
// +build unit

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

package tcsclient

import (
	"strconv"
	"testing"

	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These constants mirror the exact string constants that the linux-only GPU
// converter (ecs-agent/gpu/gpu_metrics_conversion.go) uses to build the
// instance-level GPU payload. We redeclare them here (with local, non-colliding
// names) rather than importing the gpu package, because that package's
// GPUMetricsToInstancePayload/ExtractInstanceGPUPayloadValues are //go:build
// linux only. Importing them would break this suite's non-linux build (this
// file carries only the "unit" build tag and runs on all GOOS).
const (
	gpuInstanceLimitMetricName = "InstanceGPULimit"
	gpuInstanceUsageMetricName = "InstanceGPUUsageTotal"
	gpuMetricUnitCountLocal    = "Count"
)

// buildInstanceGPUMetrics constructs an *ecstcs.InstanceMetrics whose
// GeneralMetricsPayload is byte-for-byte identical to what the linux converter
// GPUMetricsToInstancePayload emits: a single dimensionless
// *ecstcs.GeneralMetricsWrapper carrying exactly two *ecstcs.GeneralMetric
// (InstanceGPULimit, InstanceGPUUsageTotal), each with Unit "Count".
func buildInstanceGPUMetrics(limit, usage int64) *ecstcs.InstanceMetrics {
	return &ecstcs.InstanceMetrics{
		GeneralMetricsPayload: []*ecstcs.GeneralMetricsWrapper{
			{
				GeneralMetrics: []*ecstcs.GeneralMetric{
					{
						MetricName:      aws.String(gpuInstanceLimitMetricName),
						MetricValueLong: aws.Int64(limit),
						Unit:            aws.String(gpuMetricUnitCountLocal),
					},
					{
						MetricName:      aws.String(gpuInstanceUsageMetricName),
						MetricValueLong: aws.Int64(usage),
						Unit:            aws.String(gpuMetricUnitCountLocal),
					},
				},
			},
		},
	}
}

// buildGPUTaskMetrics returns n TaskMetric objects with distinct TaskArns.
func buildGPUTaskMetrics(n int) []*ecstcs.TaskMetric {
	var taskMetrics []*ecstcs.TaskMetric
	for i := 0; i < n; i++ {
		taskArn := "task/" + strconv.Itoa(i)
		taskMetrics = append(taskMetrics, &ecstcs.TaskMetric{TaskArn: aws.String(taskArn)})
	}
	return taskMetrics
}

// nonIdleGPUMetadata returns a non-idle MetricsMetadata for GPU instance tests.
func nonIdleGPUMetadata() *ecstcs.MetricsMetadata {
	return &ecstcs.MetricsMetadata{
		Cluster:           aws.String(testCluster),
		ContainerInstance: aws.String(testContainerInstance),
		Idle:              aws.Bool(false),
		MessageId:         aws.String(testMessageId),
	}
}

// extractInstanceGPUValues parses the InstanceGPULimit and InstanceGPUUsageTotal
// values back out of an *ecstcs.InstanceMetrics. This is a local re-implementation
// of the linux-only ExtractInstanceGPUPayloadValues helper so this suite stays
// buildable on all platforms.
func extractInstanceGPUValues(im *ecstcs.InstanceMetrics) (limit int64, usage int64, ok bool) {
	if im == nil || len(im.GeneralMetricsPayload) == 0 {
		return 0, 0, false
	}
	wrapper := im.GeneralMetricsPayload[0]
	var gotLimit, gotUsage bool
	for _, m := range wrapper.GeneralMetrics {
		if m == nil || m.MetricName == nil || m.MetricValueLong == nil {
			continue
		}
		switch aws.ToString(m.MetricName) {
		case gpuInstanceLimitMetricName:
			limit = *m.MetricValueLong
			gotLimit = true
		case gpuInstanceUsageMetricName:
			usage = *m.MetricValueLong
			gotUsage = true
		}
	}
	return limit, usage, gotLimit && gotUsage
}

// countRequestsWithInstanceMetrics returns how many of the given requests carry
// a non-nil InstanceMetrics.
func countRequestsWithInstanceMetrics(reqs []*ecstcs.PublishMetricsRequest) int {
	count := 0
	for _, req := range reqs {
		if req.InstanceMetrics != nil {
			count++
		}
	}
	return count
}

// TestGPUInstanceMetrics_RideInFirstRequest_SingleRequest proves the exact
// *ecstcs.InstanceMetrics object the engine attaches survives
// metricsToPublishMetricRequests intact and lands name/value/unit-correct on the
// single outbound PublishMetricsRequest.
func TestGPUInstanceMetrics_RideInFirstRequest_SingleRequest(t *testing.T) {
	cs := tcsClientServer{}
	msg := ecstcs.TelemetryMessage{
		InstanceMetrics: buildInstanceGPUMetrics(4, 2),
		Metadata:        nonIdleGPUMetadata(),
		TaskMetrics:     buildGPUTaskMetrics(3), // 3 < tasksInMetricMessage(10), one page
	}
	requests, err := cs.metricsToPublishMetricRequests(msg)
	require.NoError(t, err)
	require.Len(t, requests, 1)

	require.NotNil(t, requests[0].InstanceMetrics)
	require.Len(t, requests[0].InstanceMetrics.GeneralMetricsPayload, 1)
	// Confirms this is the dimensionless instance wrapper, not a per-container one.
	assert.Empty(t, requests[0].InstanceMetrics.GeneralMetricsPayload[0].Dimensions)

	limit, usage, ok := extractInstanceGPUValues(requests[0].InstanceMetrics)
	assert.True(t, ok)
	assert.Equal(t, int64(4), limit)
	assert.Equal(t, int64(2), usage)

	for _, m := range requests[0].InstanceMetrics.GeneralMetricsPayload[0].GeneralMetrics {
		assert.Equal(t, gpuMetricUnitCountLocal, aws.ToString(m.Unit))
	}

	// Single request is also the last, so Fin must be set.
	assert.True(t, aws.ToBool(requests[0].Metadata.Fin))
}

// TestGPUInstanceMetrics_OnlyInFirstOfMultipleRequests proves that when a
// publish cycle splits into multiple wire requests (by task count OR byte size),
// the GPU instance payload is emitted exactly once and specifically on the first
// frame.
func TestGPUInstanceMetrics_OnlyInFirstOfMultipleRequests(t *testing.T) {
	testCases := []struct {
		name             string
		splitBySize      bool
		numTasks         int
		expectedRequests int
		limit            int64
		usage            int64
	}{
		{
			name: "split by task count",
			// 21 tasks => 3 batches of {10, 10, 1}
			numTasks:         (tasksInMetricMessage * 2) + 1,
			expectedRequests: 3,
			limit:            8,
			usage:            5,
		},
		{
			name:             "split by size",
			splitBySize:      true,
			numTasks:         3,
			expectedRequests: 2,
			limit:            2,
			usage:            1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.splitBySize {
				tempLimit := publishMetricRequestSizeLimit
				publishMetricRequestSizeLimit = testPublishMetricRequestSizeLimitNonSCWithInstanceMetrics
				defer func() {
					publishMetricRequestSizeLimit = tempLimit
				}()
			}

			cs := tcsClientServer{}
			requests, err := cs.metricsToPublishMetricRequests(ecstcs.TelemetryMessage{
				InstanceMetrics: buildInstanceGPUMetrics(tc.limit, tc.usage),
				Metadata:        nonIdleGPUMetadata(),
				TaskMetrics:     buildGPUTaskMetrics(tc.numTasks),
			})
			require.NoError(t, err)
			require.Len(t, requests, tc.expectedRequests)

			for i, req := range requests {
				if i == 0 {
					require.NotNil(t, req.InstanceMetrics)
				} else {
					assert.Nil(t, req.InstanceMetrics, "instance metrics must only ride in the first request")
				}
			}
			assert.Equal(t, 1, countRequestsWithInstanceMetrics(requests))

			limit, usage, ok := extractInstanceGPUValues(requests[0].InstanceMetrics)
			assert.True(t, ok)
			assert.Equal(t, tc.limit, limit)
			assert.Equal(t, tc.usage, usage)

			// Fin only on the last request.
			assert.True(t, aws.ToBool(requests[len(requests)-1].Metadata.Fin))
			for i := 0; i < len(requests)-1; i++ {
				assert.False(t, aws.ToBool(requests[i].Metadata.Fin))
			}

			// Confirm splitting is well-formed: no task lost or duplicated.
			taskArns := make(map[string]bool)
			for _, req := range requests {
				for _, tm := range req.TaskMetrics {
					assert.False(t, taskArns[aws.ToString(tm.TaskArn)], "duplicate task arn: %s", aws.ToString(tm.TaskArn))
					taskArns[aws.ToString(tm.TaskArn)] = true
				}
			}
			assert.Equal(t, tc.numTasks, len(taskArns))
		})
	}
}

// TestGPUInstanceMetrics_IdlePathCarriesInstanceMetrics is a defensive
// lower-level contract test of the idle branch (client.go idle short-circuit)
// which bypasses filterInstanceMetrics by building a single request directly via
// NewPublishMetricsRequest. It does not assert the engine ever produces idle+GPU
// together; that is covered end-to-end by the engine idle-emits test.
func TestGPUInstanceMetrics_IdlePathCarriesInstanceMetrics(t *testing.T) {
	cs := tcsClientServer{}
	metadata := &ecstcs.MetricsMetadata{
		Cluster:           aws.String(testCluster),
		ContainerInstance: aws.String(testContainerInstance),
		Idle:              aws.Bool(true),
		MessageId:         aws.String(testMessageId),
	}
	msg := ecstcs.TelemetryMessage{
		InstanceMetrics: buildInstanceGPUMetrics(1, 0),
		Metadata:        metadata,
		TaskMetrics:     []*ecstcs.TaskMetric{},
	}
	requests, err := cs.metricsToPublishMetricRequests(msg)
	require.NoError(t, err)
	require.Len(t, requests, 1)

	require.NotNil(t, requests[0].InstanceMetrics)
	limit, usage, ok := extractInstanceGPUValues(requests[0].InstanceMetrics)
	assert.True(t, ok)
	assert.Equal(t, int64(1), limit)
	assert.Equal(t, int64(0), usage)

	assert.True(t, aws.ToBool(requests[0].Metadata.Fin))
}

// TestGPUInstanceMetrics_NilInstanceMetricsNeverLeaksIntoRequests is the
// negative baseline: with nil InstanceMetrics input (the steady state 2 of every
// 3 ticks and on all non-GPU instances), no request in any frame carries
// instance metrics.
func TestGPUInstanceMetrics_NilInstanceMetricsNeverLeaksIntoRequests(t *testing.T) {
	cs := tcsClientServer{}
	msg := ecstcs.TelemetryMessage{
		InstanceMetrics: nil,
		Metadata:        nonIdleGPUMetadata(),
		TaskMetrics:     buildGPUTaskMetrics(21), // 21 tasks force 3 requests
	}
	requests, err := cs.metricsToPublishMetricRequests(msg)
	require.NoError(t, err)
	require.Len(t, requests, 3)

	for _, req := range requests {
		assert.Nil(t, req.InstanceMetrics)
	}
	assert.Equal(t, 0, countRequestsWithInstanceMetrics(requests))

	taskArns := make(map[string]bool)
	for _, req := range requests {
		for _, tm := range req.TaskMetrics {
			taskArns[aws.ToString(tm.TaskArn)] = true
		}
	}
	assert.Equal(t, 21, len(taskArns))
}
