//go:build linux

package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/cihub/seelog"
)

const (
	// DefaultDCGMSocketPath is the default connection address for nv-hostengine (TCP).
	DefaultDCGMSocketPath = "localhost:5555"

	mibToBytes = 1024 * 1024
)

// metricsFieldDef pairs a DCGM field ID with its name.
type metricsFieldDef struct {
	id   dcgm.Short
	name string
}

// metricsFields defines the DCGM fields we watch (matches Two's basic set).
var metricsFields = []metricsFieldDef{
	{dcgm.DCGM_FI_DEV_UUID, "DCGM_FI_DEV_UUID"},
	{dcgm.DCGM_FI_DEV_FB_TOTAL, "DCGM_FI_DEV_FB_TOTAL"},
	{dcgm.DCGM_FI_DEV_GPU_UTIL, "DCGM_FI_DEV_GPU_UTIL"},
	{dcgm.DCGM_FI_DEV_FB_USED_PERCENT, "DCGM_FI_DEV_FB_USED_PERCENT"},
	{dcgm.DCGM_FI_DEV_FB_USED, "DCGM_FI_DEV_FB_USED"},
	{dcgm.DCGM_FI_DEV_POWER_USAGE, "DCGM_FI_DEV_POWER_USAGE"},
	{dcgm.DCGM_FI_DEV_GPU_TEMP, "DCGM_FI_DEV_GPU_TEMP"},
}

const (
	fieldIdxUUID = iota
	fieldIdxFBTotal
	fieldIdxGPUUtil
	fieldIdxMemUtil
	fieldIdxFBUsed
	fieldIdxPower
	fieldIdxTemp
)

func metricsFieldIDs() []dcgm.Short {
	ids := make([]dcgm.Short, len(metricsFields))
	for i, f := range metricsFields {
		ids[i] = f.id
	}
	return ids
}

// DCGMGPUMetric holds per-device GPU telemetry.
type DCGMGPUMetric struct {
	GPUUUID           string  `json:"gpu_uuid"`
	GPUUtilization    float64 `json:"gpu_utilization_percent"`
	MemoryUtilization float64 `json:"memory_utilization_percent"`
	MemoryTotalBytes  uint64  `json:"memory_total_bytes"`
	MemoryUsedBytes   uint64  `json:"memory_used_bytes"`
	PowerDrawWatts    float64 `json:"power_draw_watts"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
	Timestamp         string  `json:"timestamp"`
}

// DCGMClient monitors GPU health via DCGM.
type DCGMClient struct {
	socketPath         string
	cleanupFunc        func()
	connected          bool
	metricsFieldGroup  dcgm.FieldHandle
	metricsWatchActive bool
	mu                 sync.Mutex
}

// NewDCGMClient creates a new DCGM client.
func NewDCGMClient(socketPath string) *DCGMClient {
	if socketPath == "" {
		socketPath = DefaultDCGMSocketPath
	}
	return &DCGMClient{socketPath: socketPath}
}

// Connect initializes the DCGM connection with a timeout.
func (c *DCGMClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	type initResult struct {
		cleanup func()
		err     error
	}
	resultChan := make(chan initResult, 1)

	go func() {
		// connectMode: "0" = TCP (nv-hostengine default), "1" = Unix socket
		cleanup, err := dcgm.Init(dcgm.Standalone, c.socketPath, "0")
		resultChan <- initResult{cleanup: cleanup, err: err}
	}()

	select {
	case result := <-resultChan:
		if result.err != nil {
			return fmt.Errorf("dcgm init failed: %w", result.err)
		}
		c.cleanupFunc = result.cleanup
	case <-time.After(10 * time.Second):
		return fmt.Errorf("dcgm init timed out after 10s")
	}

	// Set up persistent field watches.
	fieldGroup, err := dcgm.FieldGroupCreate("ecs_gpu_metrics", metricsFieldIDs())
	if err != nil {
		seelog.Warnf("DCGM: failed to create field group: %v", err)
	} else {
		if err := dcgm.WatchFieldsWithGroup(fieldGroup, dcgm.GroupAllGPUs()); err != nil {
			seelog.Warnf("DCGM: failed to watch fields: %v", err)
		} else {
			c.metricsFieldGroup = fieldGroup
			c.metricsWatchActive = true
		}
	}

	c.connected = true
	seelog.Info("DCGM: connected to nv-hostengine")
	return nil
}

// CollectMetrics gathers GPU metrics from all devices.
func (c *DCGMClient) CollectMetrics() ([]DCGMGPUMetric, error) {
	c.mu.Lock()
	connected := c.connected
	watchActive := c.metricsWatchActive
	c.mu.Unlock()

	if !connected {
		return nil, fmt.Errorf("DCGM client not connected")
	}
	if !watchActive {
		return nil, fmt.Errorf("DCGM metrics watch not active")
	}

	gpus, err := dcgm.GetSupportedDevices()
	if err != nil {
		return nil, fmt.Errorf("failed to get devices: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	metrics := make([]DCGMGPUMetric, 0, len(gpus))

	for _, gpu := range gpus {
		values, err := dcgm.GetLatestValuesForFields(gpu, metricsFieldIDs())
		if err != nil {
			seelog.Warnf("DCGM: failed to get values for GPU %d: %v", gpu, err)
			continue
		}

		m := DCGMGPUMetric{Timestamp: now}
		extractFields(&m, values)
		metrics = append(metrics, m)
	}

	return metrics, nil
}

// CollectMetricsJSON returns metrics as a JSON string (for CloudWatch Logs).
func (c *DCGMClient) CollectMetricsJSON() (string, error) {
	metrics, err := c.CollectMetrics()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Shutdown disconnects from DCGM.
func (c *DCGMClient) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return
	}
	if c.cleanupFunc != nil {
		c.cleanupFunc()
		c.cleanupFunc = nil
	}
	c.connected = false
	c.metricsWatchActive = false
	seelog.Info("DCGM: shutdown complete")
}

func extractFields(m *DCGMGPUMetric, values []dcgm.FieldValue_v1) {
	if fieldIdxUUID < len(values) && values[fieldIdxUUID].Status == 0 {
		m.GPUUUID = values[fieldIdxUUID].String()
	}
	if fieldIdxFBTotal < len(values) && isValidInt64(values[fieldIdxFBTotal]) {
		m.MemoryTotalBytes = safeUint64(values[fieldIdxFBTotal].Int64()) * mibToBytes
	}
	if fieldIdxGPUUtil < len(values) && isValidInt64(values[fieldIdxGPUUtil]) {
		m.GPUUtilization = float64(values[fieldIdxGPUUtil].Int64())
	}
	if fieldIdxMemUtil < len(values) && isValidFloat64(values[fieldIdxMemUtil]) {
		m.MemoryUtilization = values[fieldIdxMemUtil].Float64() * 100
	}
	if fieldIdxFBUsed < len(values) && isValidInt64(values[fieldIdxFBUsed]) {
		m.MemoryUsedBytes = safeUint64(values[fieldIdxFBUsed].Int64()) * mibToBytes
	}
	if fieldIdxPower < len(values) && isValidFloat64(values[fieldIdxPower]) {
		m.PowerDrawWatts = values[fieldIdxPower].Float64()
	}
	if fieldIdxTemp < len(values) && isValidInt64(values[fieldIdxTemp]) {
		m.TemperatureCelsius = float64(values[fieldIdxTemp].Int64())
	}
}

func isValidInt64(fv dcgm.FieldValue_v1) bool {
	if fv.Status != 0 {
		return false
	}
	v := fv.Int64()
	return v != 0x7ffffff0 && v != 0x7ffffff1 && v != 0x7ffffff2 && v != 0x7ffffff3 &&
		v != 0x7ffffffffffffff0 && v != 0x7ffffffffffffff1 && v != 0x7ffffffffffffff2 && v != 0x7ffffffffffffff3
}

func isValidFloat64(fv dcgm.FieldValue_v1) bool {
	if fv.Status != 0 {
		return false
	}
	v := fv.Float64()
	return v != 140737488355328.0 && v != 140737488355329.0 && v != 140737488355330.0 && v != 140737488355331.0 && v >= 0
}

func safeUint64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
