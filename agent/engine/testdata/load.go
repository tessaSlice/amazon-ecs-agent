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

package testdata

import (
	"encoding/json"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strings"

	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/golang/mock/gomock"
)

func LoadTask(name string) *apitask.Task {
	_, filename, _, _ := runtime.Caller(0)
	filedata, err := ioutil.ReadFile(filepath.Join(filepath.Dir(filename), "test_tasks", name+".json"))
	if err != nil {
		panic(err)
	}
	t := &apitask.Task{}
	if err := json.Unmarshal(filedata, t); err != nil {
		panic(err)
	}
	return t
}

type dockerNameSubstr struct {
	values []string
}

func (m dockerNameSubstr) Matches(arg interface{}) bool {
	sarg := arg.(string)
	for _, s := range m.values {
		if !strings.Contains(sarg, s) {
			return false
		}
	}
	return true
}

// Not used here, but satisfies the Matcher interface.
func (m dockerNameSubstr) String() string {
	return strings.Join(m.values, ", ")
}

func DockerNameSubstr(values ...string) gomock.Matcher {
	return dockerNameSubstr{values: values}
}
