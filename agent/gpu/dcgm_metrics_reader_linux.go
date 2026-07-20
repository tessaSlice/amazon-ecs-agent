//go:build linux

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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	gputypes "github.com/aws/amazon-ecs-agent/ecs-agent/gpu/types"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger/field"
)

// DCGMMetricsReader reads the GPU metrics JSON file written by dcgm-init.
type DCGMMetricsReader struct {
	filePath string
}

func NewDCGMMetricsReader(filePath string) *DCGMMetricsReader {
	if filePath == "" {
		filePath = gputypes.GPUMetricsFilePath
	}
	return &DCGMMetricsReader{filePath: filePath}
}

// GetGPUMetrics reads and parses the GPU metrics file. Returns nil if the file
// is missing, unreadable, corrupt, or has an invalid timestamp.
func (r *DCGMMetricsReader) GetGPUMetrics() *gputypes.GPUMetricsFileData {
	data, err := os.ReadFile(r.filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			logger.Debug("GPU metrics file not available", logger.Fields{field.Error: err})
		} else {
			logger.Warn("GPU metrics file is not readable", logger.Fields{field.Path: r.filePath, field.Error: err})
		}
		return nil
	}

	var fileData gputypes.GPUMetricsFileData
	if err := json.Unmarshal(data, &fileData); err != nil {
		logger.Warn("Failed to parse GPU metrics file", logger.Fields{field.Error: err})
		return nil
	}

	if _, err := time.Parse(gputypes.TimestampFormat, fileData.Timestamp); err != nil {
		logger.Warn("Failed to parse GPU metrics timestamp", logger.Fields{field.Error: err})
		return nil
	}

	return &fileData
}
