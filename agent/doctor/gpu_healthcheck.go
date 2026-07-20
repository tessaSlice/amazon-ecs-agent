//go:build linux

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
//  2. ConnectionLost == true → INSUFFICIENT_DATA (health unknown).
//  3. Healthy == true → OK.
//  4. Healthy == false → IMPAIRED.
//
// This health check does not evaluate timestamp staleness. GPU metric emission
// de-duplicates only the exact timestamp most recently reported to TACS.
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
