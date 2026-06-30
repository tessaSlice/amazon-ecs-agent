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

package volume

import (
	"os"
	"path/filepath"
)

// tmpAccessor is used for testing purposes only. It implements the
// volume accessor interface to access/write files from/to the temp directory.
type tmpAccessor struct {
	// tmpDir denotes a subdirectory under /tmp which requires to be accessed.
	tmpDir string
}

func NewTmpAccessor(tmpDirName string) TaskVolumeAccessor {
	return &tmpAccessor{
		tmpDir: tmpDirName,
	}
}

// CopyToVolume copies the source file into a subdirectory under /tmp.
// Name of the subdirectory can be configured by specifying a value
// for tmpDir while creating tmpAccessor.
func (t *tmpAccessor) CopyToVolume(taskID, src, dst string, mode os.FileMode) error {
	// Read source file.
	contents, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	// Create destination directory.
	finalDstDir := filepath.Join("/tmp", t.tmpDir)
	err = os.MkdirAll(finalDstDir, mode)
	if err != nil {
		return err
	}

	finalDst := filepath.Join(finalDstDir, dst)
	return os.WriteFile(finalDst, contents, mode)
}

func (t *tmpAccessor) DeleteAll(string) error {
	return nil
}
