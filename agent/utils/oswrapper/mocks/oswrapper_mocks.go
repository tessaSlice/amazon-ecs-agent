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

package mocks

import (
	"os"

	"github.com/aws/amazon-ecs-agent/agent/utils/oswrapper"
)

type MockFile struct {
	ChmodImpl func(os.FileMode) error
	NameImpl  func() string
	SyncImpl  func() error
	WriteImpl func([]byte) (int, error)
}

func NewMockFile() oswrapper.File {
	return &MockFile{
		ChmodImpl: func(os.FileMode) error {
			return nil
		},
		NameImpl: func() string {
			return ""
		},
		SyncImpl: func() error {
			return nil
		},
		WriteImpl: func(bytes []byte) (i int, e error) {
			return 0, nil
		},
	}
}

func (f *MockFile) Name() string {
	return f.NameImpl()
}

func (f *MockFile) Close() error {
	return nil
}

func (f *MockFile) Chmod(fileMode os.FileMode) error {
	return f.ChmodImpl(fileMode)
}

func (f *MockFile) Write(content []byte) (int, error) {
	return f.WriteImpl(content)
}

func (f *MockFile) WriteAt(b []byte, off int64) (n int, err error) {
	return 0, nil
}

func (f *MockFile) Sync() error {
	return f.SyncImpl()
}

func (f *MockFile) Read([]byte) (int, error) {
	return 0, nil
}
