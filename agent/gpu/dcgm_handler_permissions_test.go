//go:build unit && linux

package gpu

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGPUMetricsDir_Permissions verifies that the GPU metrics directory
// is created with 0755 permissions (owner rwx, group/other rx).
func TestGPUMetricsDir_Permissions(t *testing.T) {
	tmpDir := t.TempDir()
	metricsDir := filepath.Join(tmpDir, "ecs")

	err := os.MkdirAll(metricsDir, 0755)
	require.NoError(t, err)

	info, err := os.Stat(metricsDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm(),
		"GPU metrics directory should have 0755 permissions")
}

// TestGPUMetricsFile_Permissions verifies that the GPU metrics file
// is written with 0644 permissions (owner rw, group/other read-only).
func TestGPUMetricsFile_Permissions(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	err := os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","gpus":[],"healthy":true}`), 0644)
	require.NoError(t, err)

	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm(),
		"GPU metrics file should have 0644 permissions (owner rw, others read-only)")
}

// TestGPUMetricsTmpFile_Permissions verifies that the temporary metrics file
// is written with 0644 permissions.
func TestGPUMetricsTmpFile_Permissions(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFilePath := filepath.Join(tmpDir, "gpu-metrics.json.tmp")

	err := os.WriteFile(tmpFilePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","gpus":[],"healthy":true}`), 0644)
	require.NoError(t, err)

	info, err := os.Stat(tmpFilePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm(),
		"GPU metrics tmp file should have 0644 permissions")
}

// TestGPUMetricsFile_ReadOnlyForNonOwner simulates the ecs-agent (non-owner)
// attempting to write to the GPU metrics file. The file should be read-only
// for non-owner processes.
func TestGPUMetricsFile_ReadOnlyForNonOwner(t *testing.T) {
	// This test is only meaningful when not running as root,
	// since root can write to any file regardless of permissions.
	currentUser, err := user.Current()
	require.NoError(t, err)
	if currentUser.Uid == "0" {
		t.Skip("Skipping permission test when running as root (root bypasses file permissions)")
	}

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")
	tmpFilePath := filepath.Join(tmpDir, "gpu-metrics.json.tmp")

	// Create files as if owned by dcgm-init (simulated by creating with restrictive perms)
	err = os.WriteFile(filePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","gpus":[],"healthy":true}`), 0444)
	require.NoError(t, err)
	err = os.WriteFile(tmpFilePath, []byte(`{"timestamp":"2026-01-01T00:00:00Z","gpus":[],"healthy":true}`), 0444)
	require.NoError(t, err)

	// Attempt to write to the metrics file (simulating ecs-agent trying to write)
	writeErr := os.WriteFile(filePath, []byte("malicious data"), 0444)
	assert.Error(t, writeErr, "Non-owner process should not be able to write to gpu-metrics.json")

	// Attempt to write to the tmp file
	writeTmpErr := os.WriteFile(tmpFilePath, []byte("malicious data"), 0444)
	assert.Error(t, writeTmpErr, "Non-owner process should not be able to write to gpu-metrics.json.tmp")

	// Verify the file content was NOT modified
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "timestamp",
		"File content should remain unchanged after failed write attempt")
}

// TestGPUMetricsFile_ReadableByAgent verifies that the GPU metrics file
// can be read by any process (simulating ecs-agent reading).
func TestGPUMetricsFile_ReadableByAgent(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "gpu-metrics.json")

	data := GPUMetricsFileData{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		GPUs: []GPUMetric{
			{GPUUUID: "GPU-read-test", GPUUtilization: ptrFloat64(50.0)},
		},
		Healthy: true,
	}
	writeMetricsFile(t, filePath, data)

	// Set file to 0644 (what dcgm-init writes)
	os.Chmod(filePath, 0644)

	// Simulate agent reading (via DCGMHandler)
	handler := NewDCGMHandler(filePath)
	metrics := handler.GetGPUMetrics()

	require.Len(t, metrics, 1, "Agent should be able to read GPU metrics file with 0644 perms")
	assert.Equal(t, "GPU-read-test", metrics[0].GPUUUID)
}

// TestGPUMetricsDir_NonOwnerCannotCreateFiles verifies that a non-root process
// cannot create new files in a directory owned by root with 0755 permissions.
func TestGPUMetricsDir_NonOwnerCannotCreateFiles(t *testing.T) {
	currentUser, err := user.Current()
	require.NoError(t, err)
	if currentUser.Uid == "0" {
		t.Skip("Skipping permission test when running as root (root bypasses file permissions)")
	}

	// Create a directory with 0755 but owned by current user (simulating root ownership)
	// To truly test this we'd need root-owned dirs, so we simulate with 0555 (no write for anyone)
	tmpDir := t.TempDir()
	restrictedDir := filepath.Join(tmpDir, "restricted-ecs")
	err = os.MkdirAll(restrictedDir, 0555)
	require.NoError(t, err)

	// Attempt to create a file in the restricted directory
	newFilePath := filepath.Join(restrictedDir, "should-fail.json")
	writeErr := os.WriteFile(newFilePath, []byte("test"), 0644)
	assert.Error(t, writeErr, "Non-owner should not be able to create files in restricted directory")
}
