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

	// A TerminalError maps to the terminal code, which RestartPreventExitStatus=5 blocks.
	terminal := engine.NewTerminalError(errors.New("bad output path"))
	assert.Equal(t, engine.TerminalFailureExitCode, exitCodeFor(terminal),
		"a *TerminalError should map to the terminal exit code")

	// A TerminalError wrapped by fmt.Errorf(%w) must still be detected via errors.As.
	wrapped := fmt.Errorf("startup failed: %w", terminal)
	assert.Equal(t, engine.TerminalFailureExitCode, exitCodeFor(wrapped),
		"a wrapped *TerminalError should still map to the terminal exit code")

	// The terminal and default codes must be distinct, or the distinction is meaningless.
	assert.NotEqual(t, engine.TerminalFailureExitCode, engine.DefaultErrorExitCode)
}
