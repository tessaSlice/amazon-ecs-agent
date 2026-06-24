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

package gpu

import (
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
)

// GPU metric names as they appear in the TACS GeneralMetric payload.
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

// GPU metric units matching CloudWatch unit conventions.
const (
	gpuMetricUnitPercent = "Percent"
	gpuMetricUnitBytes   = "Bytes"
	gpuMetricUnitNone    = "None"
	gpuMetricUnitCount   = "Count"
)

// gpuDeviceDimensionKey is the dimension key used to identify accelerated devices.
const gpuDeviceDimensionKey = "AcceleratedDevice"

// GPUMetricToGeneralMetricsWrapper converts a single GPUMetric to a GeneralMetricsWrapper.
// Only non-nil metric fields are included. Returns nil if all metric fields are nil.
func GPUMetricToGeneralMetricsWrapper(m GPUMetric) *ecstcs.GeneralMetricsWrapper {
	var generalMetrics []*ecstcs.GeneralMetric

	if m.GPUUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        new(gpuMetricNameGPUUtilization),
			MetricValueDouble: m.GPUUtilization,
			Unit:              new(gpuMetricUnitPercent),
		})
	}
	if m.MemoryUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        new(gpuMetricNameGPUMemoryUtilization),
			MetricValueDouble: m.MemoryUtilization,
			Unit:              new(gpuMetricUnitPercent),
		})
	}
	if m.MemoryTotal != nil {
		v := int64(*m.MemoryTotal) //nolint:gosec // Max GPU memory is ~141 GB (H200); int64 max is ~9.2 EB. Overflow is impossible.
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      new(gpuMetricNameGPUMemoryTotal),
			MetricValueLong: &v,
			Unit:            new(gpuMetricUnitBytes),
		})
	}
	if m.MemoryUsed != nil {
		v := int64(*m.MemoryUsed) //nolint:gosec // Max GPU memory is ~141 GB (H200); int64 max is ~9.2 EB. Overflow is impossible.
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      new(gpuMetricNameGPUMemoryUsed),
			MetricValueLong: &v,
			Unit:            new(gpuMetricUnitBytes),
		})
	}
	if m.PowerDraw != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        new(gpuMetricNameGPUPowerDraw),
			MetricValueDouble: m.PowerDraw,
			Unit:              new(gpuMetricUnitNone),
		})
	}
	if m.Temperature != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        new(gpuMetricNameGPUTemperature),
			MetricValueDouble: m.Temperature,
			Unit:              new(gpuMetricUnitNone),
		})
	}
	if len(generalMetrics) == 0 {
		return nil
	}

	// Always include RESTART_APP XID count so customers see 0 instead of "No Data".
	xidCount := m.RestartAppXidCount
	generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
		MetricName:      new(gpuMetricNameGPURestartAppXidCount),
		MetricValueLong: &xidCount,
		Unit:            new(gpuMetricUnitCount),
	})

	return &ecstcs.GeneralMetricsWrapper{
		Dimensions: []*ecstcs.Dimension{
			{
				Key:   new(gpuDeviceDimensionKey),
				Value: new(m.GPUUUID),
			},
		},
		GeneralMetrics: generalMetrics,
	}
}

// GPUMetricsToInstancePayload converts a slice of GPUMetrics to instance-level
// GeneralMetricsWrapper entries containing InstanceGPULimit and InstanceGPUUsageTotal.
// The usageTotal parameter is the pre-computed count of unique GPU device IDs assigned
// to running task containers. Returns nil if the input slice is empty.
func GPUMetricsToInstancePayload(metrics []GPUMetric, usageTotal int64) []*ecstcs.GeneralMetricsWrapper {
	if len(metrics) == 0 {
		return nil
	}

	limitCount := int64(len(metrics))

	return []*ecstcs.GeneralMetricsWrapper{
		{
			GeneralMetrics: []*ecstcs.GeneralMetric{
				{
					MetricName:      new(gpuMetricNameInstanceGPULimitCount),
					MetricValueLong: &limitCount,
					Unit:            new(gpuMetricUnitCount),
				},
				{
					MetricName:      new(gpuMetricNameInstanceGPUUsageTotal),
					MetricValueLong: &usageTotal,
					Unit:            new(gpuMetricUnitCount),
				},
			},
		},
	}
}

// ExtractInstanceGPUPayloadValues extracts InstanceGPULimit and InstanceGPUUsageTotal
// values from a GeneralMetricsWrapper slice produced by GPUMetricsToInstancePayload.
// Returns ok=false if the payload structure is unexpected.
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

// GPUMetricsForContainer returns the GeneralMetricsWrapper entries for a specific
// container based on its assigned GPU device IDs. It matches GPUMetric GPUUUID
// against the provided device ID list.
func GPUMetricsForContainer(metrics []GPUMetric, gpuDeviceIDs []string) []*ecstcs.GeneralMetricsWrapper {
	if len(metrics) == 0 || len(gpuDeviceIDs) == 0 {
		return nil
	}

	// Build a set of device IDs for O(1) lookup.
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
