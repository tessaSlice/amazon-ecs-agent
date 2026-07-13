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
	"sync"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/gpu"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/cihub/seelog"
)

// gpuMetricsMaxStaleness bounds how old the shared GPU metrics file may be before
// the health check treats it as unknown. dcgm-init rewrites the file every ~60s
// (even on collection failure), so a file older than this means dcgm-init is dead
// or hung — connection_lost cannot cover that, since a stopped dcgm-init can no
// longer update the flag. Sized to tolerate a few missed writes without flapping.
const gpuMetricsMaxStaleness = 5 * time.Minute

type gpuHealthcheck struct {
	HealthcheckType  string
	Status           ecstcs.InstanceHealthCheckStatus
	TimeStamp        time.Time
	StatusChangeTime time.Time
	LastStatus       ecstcs.InstanceHealthCheckStatus
	LastTimeStamp    time.Time

	handler *gpu.DCGMHandler
	lock    sync.RWMutex
}

// NewGPUHealthcheck creates a new GPU health check that queries the DCGMHandler
// for health status from the shared GPU metrics file written by dcgm-init.
func NewGPUHealthcheck(handler *gpu.DCGMHandler) *gpuHealthcheck {
	nowTime := timeNow()
	return &gpuHealthcheck{
		HealthcheckType:  ecstcs.InstanceHealthCheckTypeAcceleratedCompute,
		Status:           ecstcs.InstanceHealthCheckStatusInitializing,
		TimeStamp:        nowTime,
		StatusChangeTime: nowTime,
		LastTimeStamp:    nowTime,
		handler:          handler,
	}
}

// RunCheck queries the DCGMHandler for GPU health status.
func (ghc *gpuHealthcheck) RunCheck() ecstcs.InstanceHealthCheckStatus {
	healthStatus := ghc.handler.GetGPUHealthStatus()
	if healthStatus == nil {
		seelog.Debug("[GPUHealthcheck] GPU health status not available")
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	// A lost DCGM connection means health is unknown: dcgm-init's Healthy stays
	// true while disconnected, so report INSUFFICIENT_DATA rather than a false OK.
	if healthStatus.ConnectionLost {
		seelog.Info("[GPUHealthcheck] DCGM connection lost, reporting insufficient data")
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	// A stale file means dcgm-init itself is dead or hung: it rewrites the file
	// every ~60s (even on collection failure), so an old timestamp — or one we
	// cannot parse — means we can no longer trust Healthy/ConnectionLost (a
	// stopped dcgm-init cannot set ConnectionLost). Report INSUFFICIENT_DATA.
	writtenAt, err := time.Parse(time.RFC3339, healthStatus.Timestamp)
	if err != nil {
		seelog.Warnf("[GPUHealthcheck] unparseable GPU metrics timestamp %q, reporting insufficient data: %v",
			healthStatus.Timestamp, err)
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}
	if age := timeNow().Sub(writtenAt); age > gpuMetricsMaxStaleness {
		seelog.Infof("[GPUHealthcheck] GPU metrics file is stale (age %s > %s), reporting insufficient data",
			age, gpuMetricsMaxStaleness)
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

func (ghc *gpuHealthcheck) SetHealthcheckStatus(healthStatus ecstcs.InstanceHealthCheckStatus) {
	ghc.lock.Lock()
	defer ghc.lock.Unlock()
	nowTime := timeNow()
	if ghc.Status != healthStatus {
		ghc.StatusChangeTime = nowTime
	}
	ghc.LastStatus = ghc.Status
	ghc.LastTimeStamp = ghc.TimeStamp
	ghc.Status = healthStatus
	ghc.TimeStamp = nowTime
}

func (ghc *gpuHealthcheck) GetHealthcheckType() string {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.HealthcheckType
}

func (ghc *gpuHealthcheck) GetHealthcheckStatus() ecstcs.InstanceHealthCheckStatus {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.Status
}

func (ghc *gpuHealthcheck) GetHealthcheckTime() time.Time {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.TimeStamp
}

func (ghc *gpuHealthcheck) GetStatusChangeTime() time.Time {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.StatusChangeTime
}

func (ghc *gpuHealthcheck) GetLastHealthcheckStatus() ecstcs.InstanceHealthCheckStatus {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.LastStatus
}

func (ghc *gpuHealthcheck) GetLastHealthcheckTime() time.Time {
	ghc.lock.RLock()
	defer ghc.lock.RUnlock()
	return ghc.LastTimeStamp
}
