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

package uuid

import (
	"fmt"

	"github.com/google/uuid"
)

const (
	DefaultSeparator = "-"
)

// GenerateWithPrefix returns a unique string with the provided prefix and separator.
func GenerateWithPrefix(prefix string, separator string) string {
	if len(prefix) > 0 {
		return fmt.Sprintf("%s%s%s", prefix, separator, uuid.New().String())
	}
	return uuid.New().String()
}
