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

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aws/amazon-ecs-agent/dcgm-init/engine"
	"github.com/aws/amazon-ecs-agent/dcgm-init/version"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/cihub/seelog"
)

const (
	START   = "start"
	VERSION = "version"
)

func main() {
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	logger.InitSeelog()
	defer seelog.Flush()

	logger.Info("dcgm-init invoked", logger.Fields{"command": args[0]})

	// version short-circuits before creating the engine: it only prints build
	// info and must not require a DCGM connection or any flags.
	if args[0] == VERSION {
		if err := version.PrintVersion(); err != nil {
			logger.Error("failed to print version info", logger.Fields{"error": err})
			seelog.Flush()
			os.Exit(1)
		}
		return
	}

	eng := engine.New()
	actions := actions(eng)

	action, ok := actions[args[0]]
	if !ok {
		usage()
		// Flush explicitly: os.Exit skips the deferred seelog.Flush(), which
		// would otherwise drop the buffered "dcgm-init invoked" log above.
		seelog.Flush()
		os.Exit(1)
	}

	if err := action.function(); err != nil {
		die(err, exitCodeFor(err))
	}
}

// exitCodeFor maps an action error to a process exit code. An unrecoverable
// failure (errors.Is engine.ErrOutputDirUnusable) maps to
// engine.RestartPreventExitCode so the systemd unit's RestartPreventExitStatus=5
// stops it from restart-looping; everything else maps to
// engine.DefaultErrorExitCode, which Restart=on-failure will retry.
func exitCodeFor(err error) int {
	if errors.Is(err, engine.ErrOutputDirUnusable) {
		return engine.RestartPreventExitCode
	}
	return engine.DefaultErrorExitCode
}

type action struct {
	function    func() error
	description string
}

func actions(eng *engine.Engine) map[string]action {
	return map[string]action{
		START: {
			function:    eng.Start,
			description: "Start collecting GPU metrics",
		},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s COMMAND\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, " Available commands:\n")
	for cmd, a := range actions(nil) {
		fmt.Fprintf(os.Stderr, "  %-10s  %s\n", cmd, a.description)
	}
	// version is handled outside the actions map because it needs no engine.
	fmt.Fprintf(os.Stderr, "  %-10s  %s\n", VERSION, "Print the dcgm-init version and exit")
	fmt.Fprintf(os.Stderr, "\n")
}

func die(err error, exitCode int) {
	logger.Error("dcgm-init failed", logger.Fields{"error": err, "exitCode": exitCode})
	seelog.Flush()
	os.Exit(exitCode)
}
