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

package utils

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestGetNumCPU(t *testing.T) {
	testCases := []struct {
		win32APIReturn      uint32
		runtimeNumCPUReturn int
		expectedAnswer      int
		name                string
	}{
		{128, 64, 128, "Both are valid"},
		{0, 64, 64, "win32 API invalid"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				win32APIGetAllActiveProcessorCount = windows.GetActiveProcessorCount
				golangRuntimeNumCPU = runtime.NumCPU
			}()

			win32APIGetAllActiveProcessorCount = func(groupNumber uint16) (ret uint32) {
				return tc.win32APIReturn
			}

			golangRuntimeNumCPU = func() int {
				return tc.runtimeNumCPUReturn
			}

			assert.Equal(t, tc.expectedAnswer, GetNumCPU(), tc.name)
		})
	}

}
