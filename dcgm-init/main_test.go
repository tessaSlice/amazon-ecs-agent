//go:build unit && linux

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

package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/amazon-ecs-agent/dcgm-init/engine"
	"github.com/stretchr/testify/assert"
)

func TestExitCodeFor(t *testing.T) {
	// A plain (transient) error maps to the default code, which systemd retries.
	assert.Equal(t, engine.DefaultErrorExitCode, exitCodeFor(errors.New("boom")),
		"a generic error should map to the default (retryable) exit code")

	// ErrOutputDirUnusable maps to the restart-prevent code, which
	// RestartPreventExitStatus=5 blocks.
	assert.Equal(t, engine.RestartPreventExitCode, exitCodeFor(engine.ErrOutputDirUnusable),
		"ErrOutputDirUnusable should map to the restart-prevent exit code")

	// The sentinel wrapped by fmt.Errorf(%w) must still be detected via errors.Is.
	wrapped := fmt.Errorf("startup failed: %w", engine.ErrOutputDirUnusable)
	assert.Equal(t, engine.RestartPreventExitCode, exitCodeFor(wrapped),
		"a wrapped ErrOutputDirUnusable should still map to the restart-prevent exit code")

	// The restart-prevent and default codes must be distinct, or the distinction is meaningless.
	assert.NotEqual(t, engine.RestartPreventExitCode, engine.DefaultErrorExitCode)
}
