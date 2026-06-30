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

package arn

import (
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/pkg/errors"
)

const (
	arnResourceDelimiter = "/"
)

// TaskIdFromArn derives the task id from the task arn
// Reference: http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html#arn-syntax-ecs
func TaskIdFromArn(taskArn string) (string, error) {
	// Parse taskARN
	parsedARN, err := arn.Parse(taskArn)
	if err != nil {
		return "", err
	}

	// Get task resource section
	resource := parsedARN.Resource

	if !strings.Contains(resource, arnResourceDelimiter) {
		return "", errors.New("malformed task ARN resource")
	}

	resourceSplit := strings.Split(resource, arnResourceDelimiter)

	return resourceSplit[len(resourceSplit)-1], nil
}
