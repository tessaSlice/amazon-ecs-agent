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

package platform

import (
	"github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/tasknetworkconfig"
)

// containerdDebug implements platform API methods for non-firecrakcer infrastructure.
type containerdDebug struct {
	containerd
}

func (c *containerdDebug) CreateDNSConfig(taskID string, netNS *tasknetworkconfig.NetworkNamespace) error {
	// SC tasks will get DNS config from control plane
	if netNS.ServiceConnectConfig != nil {
		return c.createDNSConfig(taskID, false, netNS)
	}
	return c.createDNSConfig(taskID, true, netNS)
}
