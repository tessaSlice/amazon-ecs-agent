//go:build linux

package gpu

import (
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
)

// GPU metric names as they appear in the TACS GeneralMetric payload.
const (
	gpuMetricNameGPUUtilization        = "GPUUtilization"
	gpuMetricNameGPUMemoryUtilization  = "GPUMemoryUtilization"
	gpuMetricNameGPUMemoryTotal        = "GPUMemoryTotal"
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

func strPtr(s string) *string { return &s }

// GPUMetricToGeneralMetricsWrapper converts a single GPUMetric to a GeneralMetricsWrapper.
// Only non-nil metric fields are included. Returns nil if all metric fields are nil.
func GPUMetricToGeneralMetricsWrapper(m GPUMetric) *ecstcs.GeneralMetricsWrapper {
	var generalMetrics []*ecstcs.GeneralMetric

	if m.GPUUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        strPtr(gpuMetricNameGPUUtilization),
			MetricValueDouble: m.GPUUtilization,
			Unit:              strPtr(gpuMetricUnitPercent),
		})
	}
	if m.MemoryUtilization != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        strPtr(gpuMetricNameGPUMemoryUtilization),
			MetricValueDouble: m.MemoryUtilization,
			Unit:              strPtr(gpuMetricUnitPercent),
		})
	}
	if m.MemoryTotal != nil {
		v := int64(*m.MemoryTotal) //nolint:gosec
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:      strPtr(gpuMetricNameGPUMemoryTotal),
			MetricValueLong: &v,
			Unit:            strPtr(gpuMetricUnitBytes),
		})
	}
	if m.PowerDraw != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        strPtr(gpuMetricNameGPUPowerDraw),
			MetricValueDouble: m.PowerDraw,
			Unit:              strPtr(gpuMetricUnitNone),
		})
	}
	if m.Temperature != nil {
		generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
			MetricName:        strPtr(gpuMetricNameGPUTemperature),
			MetricValueDouble: m.Temperature,
			Unit:              strPtr(gpuMetricUnitNone),
		})
	}
	if len(generalMetrics) == 0 {
		return nil
	}

	xidCount := m.RestartAppXidCount
	generalMetrics = append(generalMetrics, &ecstcs.GeneralMetric{
		MetricName:      strPtr(gpuMetricNameGPURestartAppXidCount),
		MetricValueLong: &xidCount,
		Unit:            strPtr(gpuMetricUnitCount),
	})

	uuid := m.GPUUUID
	return &ecstcs.GeneralMetricsWrapper{
		Dimensions: []*ecstcs.Dimension{
			{
				Key:   strPtr(gpuDeviceDimensionKey),
				Value: &uuid,
			},
		},
		GeneralMetrics: generalMetrics,
	}
}

// GPUMetricsToInstancePayload converts a slice of GPUMetrics to instance-level
// GeneralMetricsWrapper entries containing InstanceGPULimit and InstanceGPUUsageTotal.
func GPUMetricsToInstancePayload(metrics []GPUMetric, usageTotal int64) []*ecstcs.GeneralMetricsWrapper {
	if len(metrics) == 0 {
		return nil
	}

	limitCount := int64(len(metrics))

	return []*ecstcs.GeneralMetricsWrapper{
		{
			GeneralMetrics: []*ecstcs.GeneralMetric{
				{
					MetricName:      strPtr(gpuMetricNameInstanceGPULimitCount),
					MetricValueLong: &limitCount,
					Unit:            strPtr(gpuMetricUnitCount),
				},
				{
					MetricName:      strPtr(gpuMetricNameInstanceGPUUsageTotal),
					MetricValueLong: &usageTotal,
					Unit:            strPtr(gpuMetricUnitCount),
				},
			},
		},
	}
}

// GPUMetricsForContainer returns the GeneralMetricsWrapper entries for a specific
// container based on its assigned GPU device IDs.
func GPUMetricsForContainer(metrics []GPUMetric, gpuDeviceIDs []string) []*ecstcs.GeneralMetricsWrapper {
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
