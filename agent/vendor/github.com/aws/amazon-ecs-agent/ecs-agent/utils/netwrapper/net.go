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

package netwrapper

import (
	n "net"
)

type Net interface {
	Interfaces() ([]n.Interface, error)
	InterfaceByName(intfName string) (*n.Interface, error)
	Addrs(intf *n.Interface) ([]n.Addr, error)
}

type net struct {
}

func NewNet() Net {
	return &net{}
}

func (*net) Interfaces() ([]n.Interface, error) {
	return n.Interfaces()
}

func (*net) InterfaceByName(intfName string) (*n.Interface, error) {
	return n.InterfaceByName(intfName)
}
func (*net) Addrs(intf *n.Interface) ([]n.Addr, error) {
	return intf.Addrs()
}
