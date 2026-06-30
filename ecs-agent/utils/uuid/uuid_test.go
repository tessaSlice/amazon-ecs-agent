//go:build unit
// +build unit

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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateUuidWithPrefix(t *testing.T) {
	for _, testCase := range []struct {
		prefix         string
		separator      string
		expectedPrefix string
	}{
		{
			prefix:         "prefix",
			separator:      DefaultSeparator,
			expectedPrefix: "prefix-",
		},
		{
			prefix:         "prefix",
			separator:      "+",
			expectedPrefix: "prefix+",
		},
		{
			prefix:         "prefix",
			separator:      "",
			expectedPrefix: "prefix",
		},
		{
			prefix:         "",
			separator:      DefaultSeparator,
			expectedPrefix: "",
		},
		{
			prefix:         "",
			separator:      "",
			expectedPrefix: "",
		},
	} {
		generatedUuid := GenerateWithPrefix(testCase.prefix, testCase.separator)
		assert.True(t, strings.HasPrefix(generatedUuid, testCase.expectedPrefix))
	}
}
