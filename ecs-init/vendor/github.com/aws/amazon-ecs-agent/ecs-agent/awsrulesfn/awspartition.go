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

package awsrulesfn

import "regexp"

const (
	// Partition identifiers
	AwsPartitionID      = "aws"        // AWS Standard partition.
	AwsCnPartitionID    = "aws-cn"     // AWS China partition.
	AwsUsGovPartitionID = "aws-us-gov" // AWS GovCloud (US) partition.

	DnsSuffix = "amazonaws.com" // Default DNS suffix for AWS Standard partition.
)

// GetPartitionForRegion returns an AWS partition for the region provided.
// Unlike GetPartition, this function
// 1. returns a Partition instead of a PartitionConfig
// 2. returns nil instead of falling back to the default partition (aws) if no match is found
func GetPartitionForRegion(region string) *Partition {
	for _, partition := range partitions {
		if _, ok := partition.Regions[region]; ok {
			return &partition
		}

		regionRegex := regexp.MustCompile(partition.RegionRegex)
		if regionRegex.MatchString(region) {
			return &partition
		}
	}

	return nil
}
