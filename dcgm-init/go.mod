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
	github.com/bits-and-blooms/bitset v1.24.5 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/NVIDIA/go-dcgm => ./third_party/go-dcgm

replace github.com/aws/amazon-ecs-agent/ecs-agent => ../ecs-agent
