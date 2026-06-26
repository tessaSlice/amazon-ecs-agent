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
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"
	"github.com/cihub/seelog"
)

const (
	defaultGPUMetricsFilePath = "/var/run/ecs/gpu-metrics.json"
)

type gpuMetricsFileData struct {
	Timestamp       string `json:"timestamp"`
	Healthy         bool   `json:"healthy"`
	UnhealthyReason string `json:"unhealthy_reason,omitempty"`
}

type gpuHealthcheck struct {
	HealthcheckType  string
	Status           ecstcs.InstanceHealthCheckStatus
	TimeStamp        time.Time
	StatusChangeTime time.Time
	LastStatus       ecstcs.InstanceHealthCheckStatus
	LastTimeStamp    time.Time

	filePath string
	lock     sync.RWMutex
}

// NewGPUHealthcheck creates a new GPU health check that reads health status
// from the shared GPU metrics file written by dcgm-init.
func NewGPUHealthcheck(filePath string) *gpuHealthcheck {
	if filePath == "" {
		filePath = defaultGPUMetricsFilePath
	}
	nowTime := timeNow()
	return &gpuHealthcheck{
		HealthcheckType:  ecstcs.InstanceHealthCheckTypeAcceleratedCompute,
		Status:           ecstcs.InstanceHealthCheckStatusInitializing,
		TimeStamp:        nowTime,
		StatusChangeTime: nowTime,
		LastTimeStamp:    nowTime,
		filePath:         filePath,
	}
}

// RunCheck reads the GPU metrics file and determines health status.
func (ghc *gpuHealthcheck) RunCheck() ecstcs.InstanceHealthCheckStatus {
	data, err := os.ReadFile(ghc.filePath)
	if err != nil {
		seelog.Debugf("[GPUHealthcheck] GPU metrics file not available: %v", err)
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	var fileData gpuMetricsFileData
	if err := json.Unmarshal(data, &fileData); err != nil {
		seelog.Warnf("[GPUHealthcheck] Failed to parse GPU metrics file: %v", err)
		ghc.SetHealthcheckStatus(ecstcs.InstanceHealthCheckStatusInsufficientData)
		return ecstcs.InstanceHealthCheckStatusInsufficientData
	}

	var resultStatus ecstcs.InstanceHealthCheckStatus
	if fileData.Healthy {
		resultStatus = ecstcs.InstanceHealthCheckStatusOk
	} else {
		seelog.Infof("[GPUHealthcheck] GPU reported unhealthy: %s", fileData.UnhealthyReason)
		resultStatus = ecstcs.InstanceHealthCheckStatusImpaired
	}

	ghc.SetHealthcheckStatus(resultStatus)
	return resultStatus
}

func (ghc *gpuHealthcheck) SetHealthcheckStatus(healthStatus ecstcs.InstanceHealthCheckStatus) {
	ghc.lock.Lock()
	defer ghc.lock.Unlock()
	nowTime := time.Now()
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
