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
	"testing"
	"time"

	"github.com/cihub/seelog"
	"github.com/stretchr/testify/assert"
)

func TestLogfmtFormat(t *testing.T) {
	logfmt := logfmtFormatter("")
	out := logfmt("This is my log message", seelog.InfoLvl, &LogContextMock{})
	s, ok := out.(string)
	assert.True(t, ok)
	assert.Equal(t, `level=info time=2018-10-01T01:02:03Z msg="This is my log message"
`, s)
}

func TestSeelogConfig(t *testing.T) {
	config = &logConfig{
		level:        defaultLogLevel,
		outputFormat: outputFmt,
		maxRollCount: 24,
	}
	c := seelogConfig()
	assert.Equal(t, `
<seelog type="asyncloop" minlevel="info">
	<outputs formatid="logfmt">
		<console />
		<rollingfile filename="`+logFile+`" type="date"
		 datepattern="2006-01-02-15" archivetype="none" maxrolls="24" />
	</outputs>
	<formats>
		<format id="logfmt" format="%InitLogfmt" />
	</formats>
</seelog>`, c)
}

type LogContextMock struct{}

func (l *LogContextMock) Func() string {
	return ""
}

func (l *LogContextMock) Line() int {
	return 0
}

func (l *LogContextMock) ShortPath() string {
	return ""
}

func (l *LogContextMock) FullPath() string {
	return ""
}

func (l *LogContextMock) IsValid() bool {
	return true
}

func (l *LogContextMock) CallTime() time.Time {
	return time.Date(2018, time.October, 1, 1, 2, 3, 0, time.UTC)
}

func (l *LogContextMock) CustomContext() interface{} {
	return map[string]string{}
}

func (l *LogContextMock) FileName() string {
	return "mytestmodule.go"
}
