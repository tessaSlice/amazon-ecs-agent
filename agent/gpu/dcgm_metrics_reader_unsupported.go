//go:build !linux

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
	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
)

// DCGMMetricsReader is a no-op on non-linux platforms.
type DCGMMetricsReader struct{}

func NewDCGMMetricsReader(_ string) *DCGMMetricsReader          { return &DCGMMetricsReader{} }
func (r *DCGMMetricsReader) GetGPUMetrics() *gputypes.GPUMetricsFileData { return nil }
