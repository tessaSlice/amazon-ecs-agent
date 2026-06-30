//go:build windows
// +build windows

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

package resizefs

import (
	"errors"
	"testing"

	"github.com/aws/amazon-ecs-agent/ecs-agent/daemonimages/csidriver/mounter"

	"github.com/golang/mock/gomock"
)

func TestResize(t *testing.T) {
	testCases := []struct {
		name            string
		deviceMountPath string
		expectedResult  bool
		expectedError   bool
		prepare         func(m *mounter.MockProxyMounter)
	}{
		{
			name:            "success: normal",
			deviceMountPath: "/mnt/test",
			expectedResult:  true,
			expectedError:   false,
			prepare: func(m *mounter.MockProxyMounter) {
				m.EXPECT().ResizeVolume(gomock.Eq("/mnt/test")).Return(true, nil)
			},
		},
		{
			name:            "failure: invalid device mount path",
			deviceMountPath: "/",
			expectedResult:  false,
			expectedError:   true,
			prepare: func(m *mounter.MockProxyMounter) {
				m.EXPECT().ResizeVolume(gomock.Eq("/")).Return(false, errors.New("Could not resize volume"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()

			mockProxyMounter := mounter.NewMockProxyMounter(mockCtl)
			tc.prepare(mockProxyMounter)

			r := NewResizeFs(mockProxyMounter)
			res, err := r.Resize("", tc.deviceMountPath)

			if tc.expectedError && err == nil {
				t.Fatalf("Expected error, but got no error")
			}
			if res != tc.expectedResult {
				t.Fatalf("Expected result is: %v, but got: %v", tc.expectedResult, res)
			}
		})
	}
}
