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
	"path/filepath"

	"github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/tasknetworkconfig"
	"github.com/pkg/errors"
)

type isolatedDebug struct {
	isolatedLinux
}

func (d *isolatedDebug) CreateDNSConfig(taskID string, netNS *tasknetworkconfig.NetworkNamespace) error {
	if err := d.createDNSConfig(taskID, true, netNS); err != nil {
		return err
	}

	// Backfill interface DNS fields from the copied resolv.conf so downstream
	// consumers can read DNS config from the model.
	primaryIF := netNS.GetPrimaryInterface()
	if primaryIF == nil || len(primaryIF.DomainNameServers) > 0 {
		return nil
	}

	src := filepath.Join(d.resolvConfPath, ResolveConfFileName)
	contents, err := d.ioutil.ReadFile(src)
	if err != nil {
		return errors.Wrapf(err, "unable to read %s", src)
	}
	servers, searches := parseResolvConf(contents)
	primaryIF.DomainNameServers = servers
	primaryIF.DomainNameSearchList = searches
	return nil
}
