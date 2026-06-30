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
	"github.com/aws/amazon-ecs-agent/ecs-agent/daemonimages/csidriver/mounter"
	"k8s.io/klog/v2"
)

// resizeFs provides support for resizing file systems
type resizeFs struct {
	proxy mounter.ProxyMounter
}

// NewResizeFs returns an instance of resizeFs
func NewResizeFs(p mounter.ProxyMounter) *resizeFs {
	return &resizeFs{proxy: p}
}

// Resize performs resize of file system
func (r *resizeFs) Resize(_, deviceMountPath string) (bool, error) {
	klog.V(3).InfoS("Resize - Expanding mounted volume", "deviceMountPath", deviceMountPath)
	return r.proxy.ResizeVolume(deviceMountPath)
}
