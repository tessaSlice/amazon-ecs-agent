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

package gpu

import (
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/aws/aws-sdk-go-v2/aws"
)

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
)

const (
	gpuMetricUnitPercent = "Percent"
	gpuMetricUnitBytes   = "Bytes"
	gpuMetricUnitNone    = "None"
	gpuMetricUnitCount   = "Count"
)

const gpuDeviceDimensionKey = "AcceleratedDevice"

// GPUMetricToGeneralMetricsWrapper converts a GPUMetric to a dimensioned wrapper.
// Returns nil if all metric fields are nil.
func GPUMetricToGeneralMetricsWrapper(m gputypes.GPUMetric) *ecstcs.GeneralMetricsWrapper {
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
		v := int64(*m.MemoryTotal) //nolint:gosec // Max GPU memory is ~141 GB (H200); int64 max is ~9.2 EB. Overflow is impossible.
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      aws.String(gpuMetricNameGPUMemoryTotal),
			MetricValueLong: &v,
			Unit:            aws.String(gpuMetricUnitBytes),
		})
	}
	if m.MemoryUsed != nil {
		v := int64(*m.MemoryUsed) //nolint:gosec // Max GPU memory is ~141 GB (H200); int64 max is ~9.2 EB. Overflow is impossible.
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

// GPUMetricsToInstancePayload builds the instance-level GPU wrapper with
// InstanceGPULimit and InstanceGPUUsageTotal. Returns nil if metrics is empty.
func GPUMetricsToInstancePayload(metrics []gputypes.GPUMetric, usageTotal int64) []*ecstcs.GeneralMetricsWrapper {
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

// ExtractInstanceGPUPayloadValues extracts limit and usage from a payload
// produced by GPUMetricsToInstancePayload. Returns ok=false on unexpected shape.
func ExtractInstanceGPUPayloadValues(payload []*ecstcs.GeneralMetricsWrapper) (limit int64, usage int64, ok bool) {
	if len(payload) == 0 {
		return 0, 0, false
	}
	wrapper := payload[0]
	if wrapper == nil || len(wrapper.GeneralMetrics) == 0 {
		return 0, 0, false
	}

	var foundLimit, foundUsage bool
	for _, gm := range wrapper.GeneralMetrics {
		if gm.MetricName == nil || gm.MetricValueLong == nil {
			continue
		}
		switch *gm.MetricName {
		case gpuMetricNameInstanceGPULimitCount:
			limit = *gm.MetricValueLong
			foundLimit = true
		case gpuMetricNameInstanceGPUUsageTotal:
			usage = *gm.MetricValueLong
			foundUsage = true
		}
	}
	return limit, usage, foundLimit && foundUsage
}

// GPUMetricsForContainer returns wrappers for the GPUs assigned to a container.
func GPUMetricsForContainer(metrics []gputypes.GPUMetric, gpuDeviceIDs []string) []*ecstcs.GeneralMetricsWrapper {
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
		wrapper := GPUMetricToGeneralMetricsWrapper(m)
		if wrapper != nil {
			result = append(result, wrapper)
		}
	}

	return result
}
