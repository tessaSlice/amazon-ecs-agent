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

package execcmd

import (
	"fmt"
)

// AgentMetadata holds metadata about the exec agent running inside the container (i.e. SSM Agent).
type AgentMetadata struct {
	PID          string `json:"PID"`
	DockerExecID string `json:"DockerExecID"`
	CMD          string `json:"CMD"`
}

func (md *AgentMetadata) String() string {
	return fmt.Sprintf("[PID: %s, DockerExecId: %s, CMD: %s]", md.PID, md.DockerExecID, md.CMD)
}

func (md *AgentMetadata) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"PID":          md.PID,
		"DockerExecID": md.DockerExecID,
		"CMD":          md.CMD,
	}
}

func MapToAgentMetadata(md map[string]interface{}) AgentMetadata {
	var execMD AgentMetadata
	if md == nil {
		return execMD
	}
	execMD.PID, _ = md["PID"].(string)
	execMD.DockerExecID, _ = md["DockerExecID"].(string)
	execMD.CMD, _ = md["CMD"].(string)
	return execMD
}
