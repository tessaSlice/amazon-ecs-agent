// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package mock_seelog

import (
	"sync"

	"github.com/cihub/seelog"
)

type CustomLoggerReceiver struct {
	OutputFormat        string
	Mu                  sync.Mutex
	TraceCalled         bool
	LastTraceMessage    string
	DebugCalled         bool
	LastDebugMessage    string
	InfoCalled          bool
	LastInfoMessage     string
	WarnCalled          bool
	LastWarnMessage     string
	ErrorCalled         bool
	LastErrorMessage    string
	CriticalCalled      bool
	LastCriticalMessage string
}

func (m *CustomLoggerReceiver) GetOutputFormat() string {
	return m.OutputFormat
}

func (m *CustomLoggerReceiver) Trace(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.TraceCalled = true
	m.LastTraceMessage = message
}

func (m *CustomLoggerReceiver) Debug(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.DebugCalled = true
	m.LastDebugMessage = message
}

func (m *CustomLoggerReceiver) Info(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.InfoCalled = true
	m.LastInfoMessage = message
}

func (m *CustomLoggerReceiver) Warn(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.WarnCalled = true
	m.LastWarnMessage = message
}

func (m *CustomLoggerReceiver) Error(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.ErrorCalled = true
	m.LastErrorMessage = message
}

func (m *CustomLoggerReceiver) Critical(message string) {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	m.CriticalCalled = true
	m.LastCriticalMessage = message
}

func (m *CustomLoggerReceiver) Flush() error {
	return nil
}
func (m *CustomLoggerReceiver) AfterParse(args interface{}) error {
	return nil
}

func (m *CustomLoggerReceiver) Close() error {
	return nil
}

func (m *CustomLoggerReceiver) ReceiveMessage(message string, level seelog.LogLevel,
	context seelog.LogContextInterface) error {

	switch level {
	case seelog.DebugLvl:
		m.Debug(message)
	case seelog.InfoLvl:
		m.Info(message)
	case seelog.WarnLvl:
		m.Warn(message)
	case seelog.ErrorLvl:
		m.Error(message)
	case seelog.CriticalLvl:
		m.Error(message)
	}
	return nil
}
