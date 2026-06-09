//go:build linux

package engine

import (
	"context"

	"github.com/aws/amazon-ecs-agent/ecs-init/gpu"
)

func startDCGMCollector() {
	gpu.StartDCGMCollector(context.Background())
}
