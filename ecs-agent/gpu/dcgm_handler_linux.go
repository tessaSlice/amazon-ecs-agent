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
	"encoding/json"
	"os"
	"sync"
	"time"

	seelog "github.com/cihub/seelog"
)

const (
	// DefaultGPUMetricsFilePath is the shared file where dcgm-init writes GPU metrics.
	DefaultGPUMetricsFilePath = "/var/run/ecs/gpu-metrics.json"
)

// GPUMetricsFileData represents the JSON structure written by dcgm-init.
type GPUMetricsFileData struct {
	Timestamp string      `json:"timestamp"`
	GPUs      []GPUMetric `json:"gpus"`
}

// DCGMHandler reads GPU metrics from the shared file written by dcgm-init
// and provides them to the stats engine for TACS reporting.
type DCGMHandler struct {
	filePath      string
	mu            sync.RWMutex
	lastTimestamp string
}

// NewDCGMHandler creates a new handler for reading GPU metrics from the shared file.
func NewDCGMHandler(filePath string) *DCGMHandler {
	if filePath == "" {
		filePath = DefaultGPUMetricsFilePath
	}
	return &DCGMHandler{
		filePath: filePath,
	}
}

// GetGPUMetrics returns the latest GPU metrics read from the shared file.
// Returns nil if the file is missing, stale, or unparseable.
func (h *DCGMHandler) GetGPUMetrics() []GPUMetric {
	h.mu.Lock()
	defer h.mu.Unlock()

	data, err := os.ReadFile(h.filePath)
	if err != nil {
		seelog.Debugf("GPU metrics file not available: %v", err)
		return nil
	}

	var fileData GPUMetricsFileData
	if err := json.Unmarshal(data, &fileData); err != nil {
		seelog.Warnf("Failed to parse GPU metrics file: %v", err)
		return nil
	}

	// Validate timestamp is parseable (reject corrupt files).
	if _, err := time.Parse(time.RFC3339, fileData.Timestamp); err != nil {
		seelog.Warnf("Failed to parse GPU metrics timestamp: %v", err)
		return nil
	}

	// Only emit metrics if the timestamp is strictly newer than the last read.
	// This prevents re-emitting stale data and guards against out-of-order reads.
	if fileData.Timestamp <= h.lastTimestamp {
		return nil
	}

	h.lastTimestamp = fileData.Timestamp
	return fileData.GPUs
}

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
