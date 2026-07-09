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

package types

const (
	// GPUMetricsDirPath holds the shared GPU metrics file. dcgm-init (writer)
	// and the agent (reader) share these constants so their paths cannot drift.
	// It is /var/run/ecs, the tmpfs runtime dir ECS already uses for
	// host<->container IPC; the metrics are a live per-boot snapshot with no
	// value across reboots. dcgm-init creates the dir on demand (it is tmpfs and
	// starts empty each boot) rather than the RPM owning a tmpfs path.
	GPUMetricsDirPath = "/var/run/ecs"

	// GPUMetricsFileName is the file dcgm-init writes GPU metrics to and the
	// agent reads.
	GPUMetricsFileName = "gpu-metrics.json"

	// GPUMetricsFilePath is the full path to the shared GPU metrics file.
	GPUMetricsFilePath = GPUMetricsDirPath + "/" + GPUMetricsFileName
)

// GPUMetric holds per-device GPU telemetry. This struct is used by both
// dcgm-init (for collection) and the agent (for TACS conversion).
type GPUMetric struct {
	GPUUUID            string   `json:"gpu_uuid"`
	GPUUtilization     *float64 `json:"gpu_utilization_percent,omitempty"`
	MemoryUtilization  *float64 `json:"memory_utilization_percent,omitempty"`
	MemoryTotal        *uint64  `json:"memory_total_bytes,omitempty"`
	MemoryUsed         *uint64  `json:"memory_used_bytes,omitempty"`
	PowerDraw          *float64 `json:"power_draw_watts,omitempty"`
	Temperature        *float64 `json:"temperature_celsius,omitempty"`
	RestartAppXidCount int64    `json:"restart_app_xid_count"`
}
