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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	seelog "github.com/cihub/seelog"
)

// GPUMetric is the shared type from ecs-agent/gpu/types.
type GPUMetric = gputypes.GPUMetric

const (
	// DefaultGPUMetricsFilePath is the shared file where dcgm-init writes GPU metrics.
	DefaultGPUMetricsFilePath = "/var/run/ecs/gpu-metrics.json"

	// unixROK is the read-permission mode passed to syscall.Access to test
	// whether this (ecs-agent) process can read the metrics file (R_OK).
	unixROK = 0x4
)

// GPUMetricsFileData represents the JSON structure written by dcgm-init.
type GPUMetricsFileData struct {
	Timestamp       string `json:"timestamp"`
	Healthy         bool   `json:"healthy"`
	UnhealthyReason string `json:"unhealthy_reason,omitempty"`
	// ConnectionLost indicates dcgm-init lost its DCGM/nv-hostengine connection
	// (outside its grace period). When true, Healthy is not trustworthy — dcgm-init's
	// IsHealthy() returns true while disconnected — so the reader must treat health
	// as unknown (INSUFFICIENT_DATA) rather than OK.
	ConnectionLost bool        `json:"connection_lost,omitempty"`
	GPUs           []GPUMetric `json:"gpus"`
}

// GPUMetricsResult holds the parsed GPU metrics along with the timestamp
// and health status from dcgm-init. Callers use the timestamp to detect stale data.
type GPUMetricsResult struct {
	Timestamp       string
	Healthy         bool
	UnhealthyReason string
	ConnectionLost  bool
	Metrics         []GPUMetric
}

// DCGMHandler reads GPU metrics from the shared file written by dcgm-init
// and provides them to the stats engine for TACS reporting.
type DCGMHandler struct {
	filePath string
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

// GetGPUMetrics reads and parses the GPU metrics file.
// Returns nil if the metrics directory or file is missing, unreadable, empty,
// corrupt, or has an invalid timestamp. The caller is responsible for staleness
// detection using the returned Timestamp.
//
// Before reading, it verifies (in order) that the containing directory exists,
// that the metrics file exists inside it, and that this process can read the
// file. The directory and file are provided to the agent container as a
// read-only bind mount of the host's /var/run/ecs, which dcgm-init creates and
// writes at runtime (it is not shipped in the RPM), so any of these checks
// failing means dcgm-init has not produced metrics yet; that is an expected
// transient state, logged at Debug and surfaced as nil.
func (h *DCGMHandler) GetGPUMetrics() *GPUMetricsResult {
	// 1. The containing directory must exist and be a directory.
	dir := filepath.Dir(h.filePath)
	if info, err := os.Stat(dir); err != nil {
		seelog.Debugf("GPU metrics directory not available: %v", err)
		return nil
	} else if !info.IsDir() {
		seelog.Warnf("GPU metrics directory path %s is not a directory", dir)
		return nil
	}

	// 2. The metrics file must exist inside the directory.
	if _, err := os.Stat(h.filePath); err != nil {
		seelog.Debugf("GPU metrics file not available: %v", err)
		return nil
	}

	// 3. This process must be able to read the file.
	if err := syscall.Access(h.filePath, unixROK); err != nil {
		seelog.Warnf("GPU metrics file %s is not readable: %v", h.filePath, err)
		return nil
	}

	data, err := os.ReadFile(h.filePath)
	if err != nil {
		seelog.Debugf("GPU metrics file not available: %v", err)
		return nil
	}

	// 4. The file must have content. dcgm-init creates an empty file on Start
	// before its first write, so an empty (or whitespace-only) file is the
	// expected pre-first-write state, not a corrupt one — log at Debug.
	if len(bytes.TrimSpace(data)) == 0 {
		seelog.Debug("GPU metrics file is empty")
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

	return &GPUMetricsResult{
		Timestamp:       fileData.Timestamp,
		Healthy:         fileData.Healthy,
		UnhealthyReason: fileData.UnhealthyReason,
		ConnectionLost:  fileData.ConnectionLost,
		Metrics:         fileData.GPUs,
	}
}

// GPUHealthStatus holds the health state from the GPU metrics file. Timestamp is
// the RFC3339 time dcgm-init last wrote the file, exposed so the health check can
// detect a stale file (dcgm-init dead or hung) — connection_lost only covers a
// lost DCGM connection while dcgm-init is still alive to report it.
type GPUHealthStatus struct {
	Healthy         bool
	UnhealthyReason string
	ConnectionLost  bool
	Timestamp       string
}

// GetGPUHealthStatus returns the GPU health status from the shared metrics file.
// Returns nil if the file is unavailable or corrupt.
func (h *DCGMHandler) GetGPUHealthStatus() *GPUHealthStatus {
	result := h.GetGPUMetrics()
	if result == nil {
		return nil
	}
	return &GPUHealthStatus{
		Healthy:         result.Healthy,
		UnhealthyReason: result.UnhealthyReason,
		ConnectionLost:  result.ConnectionLost,
		Timestamp:       result.Timestamp,
	}
}
