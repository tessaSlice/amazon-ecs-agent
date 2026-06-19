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

package logger

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cihub/seelog"
)

const (
	LOGLEVEL_ENV_VAR = "DCGM_INIT_LOGLEVEL"
	defaultLogLevel  = "info"
	outputFmt        = "logfmt"
)

var logLevels = map[string]string{
	"debug": "debug",
	"info":  "info",
	"warn":  "warn",
	"error": "error",
	"crit":  "critical",
	"none":  "off",
}

type logConfig struct {
	level        string
	outputFormat string
	lock         sync.Mutex
}

var config *logConfig

func init() {
	config = &logConfig{
		level:        defaultLogLevel,
		outputFormat: outputFmt,
	}
}

func Setup() {
	if err := seelog.RegisterCustomFormatter("DcgmInitLogfmt", logfmtFormatter); err != nil {
		fmt.Printf("Failed to register DcgmInitLogfmt formatter: %v\n", err)
	}

	if logLevel := os.Getenv(LOGLEVEL_ENV_VAR); logLevel != "" {
		SetLogLevel(logLevel)
	}
	reloadConfig()
}

func logfmtFormatter(params string) seelog.FormatterFunc {
	return func(message string, level seelog.LogLevel, context seelog.LogContextInterface) interface{} {
		return fmt.Sprintf("level=%s time=%s msg=%q\n",
			level.String(), context.CallTime().UTC().Format("2006-01-02T15:04:05Z"), message)
	}
}

func SetLogLevel(logLevel string) {
	parsedLevel, ok := logLevels[strings.ToLower(logLevel)]
	if ok {
		config.lock.Lock()
		defer config.lock.Unlock()
		config.level = parsedLevel
		reloadConfig()
	} else {
		seelog.Error("log level mapping not found")
	}
}

func reloadConfig() {
	logger, err := seelog.LoggerFromConfigAsString(seelogConfig())
	if err == nil {
		seelog.ReplaceLogger(logger)
	} else {
		seelog.Error(err)
	}
}

func seelogConfig() string {
	return `
<seelog type="asyncloop" minlevel="` + config.level + `">
	<outputs formatid="` + config.outputFormat + `">
		<console />
	</outputs>
	<formats>
		<format id="logfmt" format="%DcgmInitLogfmt" />
	</formats>
</seelog>`
}
