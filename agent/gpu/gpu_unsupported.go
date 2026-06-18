//go:build !linux

package gpu

import "github.com/aws/amazon-ecs-agent/ecs-agent/tcs/model/ecstcs"

type GPUMetric struct{}

type DCGMHandler struct{}

func NewDCGMHandler(_ string) *DCGMHandler { return &DCGMHandler{} }

func (h *DCGMHandler) GetGPUMetrics() []GPUMetric { return nil }

func GPUMetricsToInstancePayload(_ []GPUMetric, _ int64) []*ecstcs.GeneralMetricsWrapper {
	return nil
}

func GPUMetricsForContainer(_ []GPUMetric, _ []string) []*ecstcs.GeneralMetricsWrapper {
	return nil
}
