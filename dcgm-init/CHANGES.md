# dcgm-init: GPU Metrics Collector for ECS

## Overview

This change introduces `dcgm-init`, a standalone Go binary that collects GPU
telemetry via NVIDIA DCGM (Data Center GPU Manager) and writes metrics as JSON
to a file on disk. It is designed to run as a systemd service on ECS GPU
instances.

## What was done

### New files

| Path | Description |
|------|-------------|
| `dcgm-init/main.go` | Entry point; runs as daemon or one-shot, writes JSON metrics |
| `dcgm-init/gpu/dcgm_client.go` | DCGM client (ported from `agent/gpu/dcgm_client.go`) — handles connection, health checks, XID filtering, metrics collection |
| `dcgm-init/gpu/dcgm_client_test.go` | Unit tests for the DCGM client |
| `dcgm-init/gpu/dcgm_mock_client.go` | Mock client for testing |
| `dcgm-init/go.mod` / `go.sum` | Go module definition |
| `dcgm-init/third_party/go-dcgm/` | Vendored NVIDIA go-dcgm library (requires DCGM 4.x) |
| `dcgm-init/vendor/` | Vendored dependencies (zap, testify, etc.) |
| `dcgm-init/TESTING.md` | Testing guide with step-by-step instructions |
| `dcgm-init/E2E_TEST_REPORT.md` | E2E test results from a real GPU instance |
| `packaging/generic-rpm-integrated/dcgm-init.service` | systemd unit file |
| `scripts/gobuild-dcgm-init.sh` | Build script (modeled after `scripts/gobuild.sh`) |
| `scripts/test-dcgm-init-e2e.sh` | Automated E2E test script |

### Modified files

| Path | Change |
|------|--------|
| `Makefile` | Added `build-dcgm-init`, `test-dcgm-init` targets; included `dcgm-init` in source tarballs and `gomod`/`clean` targets |
| `packaging/generic-rpm-integrated/amazon-ecs-init.spec` | Added dcgm-init build, install, systemd unit, and %files entries |
| `packaging/amazon-linux-ami-integrated/ecs-agent.spec` | Same additions for the Amazon Linux integrated package |

## Architecture

dcgm-init uses **DCGM embedded mode** (`dcgm.Init(dcgm.Embedded)`):

- Starts an in-process nv-hostengine directly within the binary
- **No external nv-hostengine daemon needed**
- **No TCP port 5555 opened**
- **No Unix domain socket needed**
- Only requires `libdcgm.so.4` to be installed (from `datacenter-gpu-manager-4-cuda12`)

The binary is managed by systemd via `dcgm-init.service`, which:
- Only starts on GPU instances (`ConditionPathExists=/dev/nvidia0`)
- Restarts on failure with 5s backoff
- Writes metrics every 10s to `/var/run/ecs/gpu-metrics.json`

## Metrics collected

The JSON output contains per-GPU:
- `gpu_uuid` — device UUID
- `gpu_utilization_percent` — compute utilization (0–100)
- `memory_utilization_percent` — memory utilization (0–100)
- `memory_total_bytes` / `memory_used_bytes` — framebuffer memory
- `power_draw_watts` — current power consumption
- `temperature_celsius` — GPU temperature
- `restart_app_xid_count` — RESTART_APP XID errors since last tick
- `healthy` — overall GPU health status
- `unhealthy_reason` — reason if unhealthy (e.g., "XID_48")

## GPU health monitoring

The client monitors:
- **Policy violations** — XID errors, DBE, NVLink, thermal, power, PCIe
- **Well-known critical XID codes** — 18 codes (48, 79, 95, etc.) that indicate hardware failure
- **DCGM health checks** — PASS/WARN/FAIL results

## Testing

- **Unit tests**: 39 tests pass without GPU hardware (`make test-dcgm-init`)
- **E2E test**: Validated on g4dn.xlarge (Tesla T4) with AL2023 ECS-Optimized GPU AMI and DCGM 4.5.2

## Requirements

- Amazon Linux 2023 (AL2 has reached EOL and only ships DCGM 3.x)
- `datacenter-gpu-manager-4-cuda12` package installed
- NVIDIA GPU drivers loaded (`/dev/nvidia0` must exist)
