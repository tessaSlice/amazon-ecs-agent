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
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/amazon-ecs-agent/dcgm-init/engine"
	"github.com/aws/amazon-ecs-agent/dcgm-init/version"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"

	"github.com/cihub/seelog"
)

// log config
const (
	logFile                      = "/var/log/ecs/dcgm-init.log"
	logDirPermission os.FileMode = 0755
)

// all supported commands
const (
	VERSION = "version"
	START   = "start"
)

func main() {
	flag.Parse()
	args := flag.Args()

	if len(args) == 0 {
		usage(actions(nil))
		os.Exit(1)
	}

	if args[0] == VERSION {
		if err := version.PrintVersion(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to print version info: %v\n", err)
		}
		return
	}

	configureLogging()
	defer seelog.Flush()

	init, err := engine.New()
	if err != nil {
		die(err, engine.DefaultInitErrorExitCode)
	}
	seelog.Info(args[0])
	actions := actions(init)
	action, ok := actions[args[0]]
	if !ok {
		usage(actions)
		seelog.Flush()
		os.Exit(1)
	}
	err = action.function()

	if err != nil {
		if err, ok := err.(*engine.TerminalError); ok {
			die(err, engine.TerminalExitCode)
		}
		die(err, engine.DefaultInitErrorExitCode)
	}
}

// configureLogging points the shared logger at logFile so dcgm-init writes to
// /var/log/ecs/dcgm-init.log, creating the directory on demand. A MkdirAll
// failure is non-fatal: warn and skip file output (console logging is
// unaffected).
func configureLogging() {
	logger.InitSeelog()

	logger.SetInstanceLogLevel(logger.DEFAULT_LOGLEVEL)
	if level := os.Getenv(logger.LOGLEVEL_ENV_VAR); level != "" {
		logger.SetInstanceLogLevel(level)
	}

	logDir := filepath.Dir(logFile)
	if err := os.MkdirAll(logDir, logDirPermission); err != nil {
		logger.Warn("dcgm-init could not create log directory %s; not writing %s: %v", logDir, logFile, err)
		return
	}
	logger.SetConfigLogFile(logFile)
	logger.SetRolloverType("date")
}

type action struct {
	function    func() error
	description string
}

func actions(engine *engine.Engine) map[string]action {
	return map[string]action{
		START: action{
			function:    engine.Start,
			description: "Start collecting GPU metrics",
		},
	}
}

func usage(actions map[string]action) {
	fmt.Printf("Usage: %s ACTION\n", os.Args[0])
	fmt.Println("")
	fmt.Println(" Available actions:")
	for command, action := range actions {
		fmt.Printf("  %-15s  %s\n", command, action.description)
	}
	fmt.Println("")
}

func die(err error, exitCode int) {
	seelog.Error(err.Error())
	seelog.Flush()
	os.Exit(exitCode)
}
