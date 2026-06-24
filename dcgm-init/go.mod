module github.com/aws/amazon-ecs-agent/dcgm-init

go 1.25.0

toolchain go1.25.9

require (
	github.com/NVIDIA/go-dcgm v0.0.0-00010101000000-000000000000
	github.com/aws/amazon-ecs-agent/ecs-agent v0.0.0
	github.com/cihub/seelog v0.0.0-20170130134532-f561c5e57575
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/aws/aws-sdk-go v1.55.7 // indirect
	github.com/aws/aws-sdk-go-v2 v1.36.6 // indirect
	github.com/aws/smithy-go v1.24.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.5 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20231006140011-7918f672742d // indirect
	golang.org/x/sys v0.46.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/NVIDIA/go-dcgm => ./third_party/go-dcgm

replace github.com/aws/amazon-ecs-agent/ecs-agent => ../ecs-agent
