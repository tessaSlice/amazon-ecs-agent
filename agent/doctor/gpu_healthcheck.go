// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//      http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package doctor

import (
	"time"

	"github.com/aws/amazon-ecs-agent/agent/doctor/statustracker"
	"github.com/aws/amazon-ecs-agent/agent/gpu"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/cihub/seelog"
)

// gpuBootGracePeriod is how long after construction the healthcheck tolerates a
// missing metrics file before flipping to INSUFFICIENT_DATA. dcgm-init writes
// on a 60-second ticker, so the grace allows its first periodic sample to land.
// ConditionPathExists can skip dcgm-init entirely on non-GPU hosts.
const gpuBootGracePeriod = 90 * time.Second

// gpuStalenessThreshold is the maximum age of a GPU metrics snapshot before the
// healthcheck considers it stale and reports INSUFFICIENT_DATA. This prevents a
// dead dcgm-init process from leaving a perpetually stale OK/IMPAIRED verdict.
// Set to 3x the producer's 60-second tick to allow for transient write delays.
const gpuStalenessThreshold = 180 * time.Second

// gpuHealthcheck implements the ACCELERATED_COMPUTE instance health check. It
// reads the shared GPU metrics file written by dcgm-init and derives a verdict.
type gpuHealthcheck struct {
	reader    *gpu.DCGMMetricsReader
	createdAt time.Time
	*statustracker.HealthCheckStatusTracker
}

// NewGPUHealthcheck creates a new GPU health check backed by the shared metrics
// file (via DCGMMetricsReader). The check starts INITIALIZING and derives a
// data-based status on the first successful read.
func NewGPUHealthcheck(reader *gpu.DCGMMetricsReader) *gpuHealthcheck {
	return &gpuHealthcheck{
		reader:                   reader,
		createdAt:                timeNow(),
		HealthCheckStatusTracker: statustracker.NewHealthCheckStatusTracker(),
	}
}

// GetHealthcheckType returns the constant ACCELERATED_COMPUTE type string.
func (ghc *gpuHealthcheck) GetHealthcheckType() string {
	return ecstcs.InstanceHealthCheckTypeAcceleratedCompute
}

// RunCheck reads the shared GPU metrics file and derives the health status.
//
// Decision order (first match wins):
//  1. File unreadable/missing/corrupt (reader returns nil) → INSUFFICIENT_DATA
//     (with a 90s boot grace while still INITIALIZING).
//  2. Timestamp stale (older than gpuStalenessThreshold) → INSUFFICIENT_DATA.
//  3. ConnectionLost == true → INSUFFICIENT_DATA (health unknown).
//  4. Healthy == true → OK.
//  5. Healthy == false → IMPAIRED.
func (ghc *gpuHealthcheck) RunCheck() ecstcs.InstanceHealthCheckStatus {
	healthStatus := ghc.reader.GetGPUMetrics()
	if healthStatus == nil {
		if ghc.GetHealthcheckStatus() == ecstcs.InstanceHealthCheckStatusInitializing &&
			timeNow().Sub(ghc.createdAt) < gpuBootGracePeriod {
			seelog.Debug("[GPUHealthcheck] GPU health status not yet available (within boot grace)")
			return ecstcs.InstanceHealthCheckStatusInitializing
		}
		seelog.Debug("[GPUHealthcheck] GPU health status not available")
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	// Detect a dead producer: if the timestamp is older than the staleness threshold,
	// the file is outdated and the verdict is unreliable.
	if ts, err := time.Parse(time.RFC3339, healthStatus.Timestamp); err == nil {
		if timeNow().Sub(ts) > gpuStalenessThreshold {
			seelog.Infof("[GPUHealthcheck] GPU metrics file is stale (age %v > %v)", timeNow().Sub(ts), gpuStalenessThreshold)
			ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
			return ecstcs.InstanceHealthCheckStatusInsufficientData
		}
	}

	if healthStatus.ConnectionLost {
		seelog.Info("[GPUHealthcheck] DCGM connection lost, reporting insufficient data")
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	var resultStatus ecstcs.InstanceHealthCheckStatus
	if healthStatus.Healthy {
		resultStatus = ecstcs.InstanceHealthCheckStatusOk
	} else {
		seelog.Infof("[GPUHealthcheck] GPU reported unhealthy: %s", healthStatus.UnhealthyReason)
		resultStatus = ecstcs.InstanceHealthCheckStatusImpaired
	}

	ghc.SetHealthcheckStatus(resultStatus)
	return resultStatus
}
