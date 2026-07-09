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
	// GPUMetricsDirPath is the directory holding the shared GPU metrics file.
	// dcgm-init (the writer) and the agent (the reader) both reference these
	// constants so the write path and the agent's bind-mount/read path cannot
	// drift apart. It lives under /var/run/ecs (tmpfs), the same runtime
	// directory ECS already uses for host<->container IPC (CSI driver socket,
	// service-connect state): the metrics are a live per-boot snapshot that is
	// regenerated continuously and has no value across a reboot. Because /var/run
	// is tmpfs and starts empty each boot, dcgm-init creates this directory on
	// demand (os.MkdirAll) rather than relying on the RPM to own a tmpfs path.
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
