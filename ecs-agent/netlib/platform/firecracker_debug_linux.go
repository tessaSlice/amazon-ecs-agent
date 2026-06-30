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

import "github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/tasknetworkconfig"

type firecrackerDebug struct {
	firecraker
}

func (fc *firecrackerDebug) CreateDNSConfig(taskID string, netNS *tasknetworkconfig.NetworkNamespace) error {
	err := fc.common.createDNSConfig(taskID, true, netNS)
	if err != nil {
		return err
	}

	return fc.configureSecondaryDNSConfig(taskID, netNS)
}
