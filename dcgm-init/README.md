# dcgm-init: Standalone GPU Metrics Collector for ECS

## Overview

`dcgm-init` is a standalone daemon that collects GPU profiling metrics from NVIDIA DCGM and exposes them via a Unix socket for the ECS Agent to consume and publish to CloudWatch.

### Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  EC2 GPU Instance (host)                                        │
│                                                                 │
│  ┌──────────────┐    go-dcgm    ┌──────────────────────┐       │
│  │ nv-hostengine│◄──────────────│  dcgm-init           │       │
│  │ (port 5555)  │   (CGO)       │  (systemd service)   │       │
│  └──────────────┘               └──────────┬───────────┘       │
│                                            │ JSON every 30s    │
│                                            ▼                    │
│                          /var/run/ecs/gpu-metrics.sock           │
│                                            │                    │
│  ┌─────────────────────────────────────────┼──────────────┐    │
│  │  ECS Agent container (scratch, CGO=0)   │              │    │
│  │                                         ▼              │    │
│  │  ┌─────────────────────────────────────────────┐       │    │
│  │  │  gpu/dcgm_reader_linux.go                   │       │    │
│  │  │  (pure Go socket client → CloudWatch)       │       │    │
│  │  └─────────────────────────────────────────────┘       │    │
│  └────────────────────────────────────────────────────────┘    │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
                    CloudWatch (ECS/GPUMetrics)
```

## Repositories Modified

### 1. amazon-ecs-agent — branch `dcgm-init-go-bindings-poc`

| File | Change | Description |
|------|--------|-------------|
| `dcgm-init/main.go` | New | Standalone DCGM collector binary. Connects to nv-hostengine, watches profiling + basic GPU fields, writes JSON to Unix socket. |
| `dcgm-init/go.mod` | New | Go module for dcgm-init with go-dcgm and seelog dependencies. |
| `dcgm-init/vendor/` | New | Vendored dependencies (go-dcgm, seelog, bitset). |
| `agent/gpu/dcgm_reader_linux.go` | New | Socket client that connects to dcgm-init, reads metrics JSON, publishes to CloudWatch. Pure Go (CGO=0). |
| `agent/agent.go` | Modified | Added `gpu.StartDCGMMetricsReader(ctx)` call at startup. |
| `agent/go.mod` | Modified | Added `aws-sdk-go-v2/service/cloudwatch` dependency. |
| `agent/vendor/` | Modified | Vendored cloudwatch SDK. |

### 2. amazon-ecs-ami — branch `dcgm-init-go-bindings-poc`

| File | Change | Description |
|------|--------|-------------|
| `scripts/al2023/gpu/install-nvidia-driver.sh` | Modified | Installs `datacenter-gpu-manager-4-core` (~27 MB) which auto-pulls `datacenter-gpu-manager-4-proprietary` (~22 MB). |

## GPU Metrics Collected

| Metric | DCGM Field | Type | Description |
|--------|-----------|------|-------------|
| GPUUtilization | DCGM_FI_DEV_GPU_UTIL | Basic | GPU compute utilization % |
| GPUMemoryUtilization | DCGM_FI_DEV_MEM_COPY_UTIL | Basic | Memory copy engine utilization % |
| GPUMemoryUsed | DCGM_FI_DEV_FB_USED | Basic | Framebuffer memory used (bytes) |
| GPUMemoryTotal | DCGM_FI_DEV_FB_TOTAL | Basic | Framebuffer memory total (bytes) |
| GPUPowerDraw | DCGM_FI_DEV_POWER_USAGE | Basic | Power consumption (watts) |
| GPUTemperature | DCGM_FI_DEV_GPU_TEMP | Basic | GPU temperature (°C) |
| **GPUSMActive** | DCGM_FI_PROF_SM_ACTIVE | **Profiling** | Fraction of SMs actively executing a warp |
| **GPUSMOccupancy** | DCGM_FI_PROF_SM_OCCUPANCY | **Profiling** | Ratio of active warps to max warps per SM |
| **GPUDRAMActive** | DCGM_FI_PROF_DRAM_ACTIVE | **Profiling** | Fraction of cycles with DRAM reads/writes |

## Packages Required on AMI

```bash
sudo dnf install -y datacenter-gpu-manager-4-core
# Automatically installs datacenter-gpu-manager-4-proprietary as dependency
```

- `datacenter-gpu-manager-4-core` (27 MB): nv-hostengine, libdcgm.so.4, dcgmi
- `datacenter-gpu-manager-4-proprietary` (22 MB): libdcgmmoduleprofiling.so.4 (**required for profiling metrics**)

Total AMI size increase: ~50 MB on a 30 GB AMI (0.17%).

## Build Instructions

### dcgm-init

```bash
cd amazon-ecs-agent/dcgm-init
CGO_ENABLED=1 CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' \
  go build -o dcgm-init .
```

Produces an 8.8 MB binary. Requires `libdcgm.so.4` on the host at runtime (provided by `datacenter-gpu-manager-4-core`).

### ecs-agent (unchanged build process)

```bash
cd amazon-ecs-agent/agent
CGO_ENABLED=0 go build -ldflags "-s" -o amazon-ecs-agent .
```

Agent remains a static binary (77 MB) — no CGO required. The socket reader is pure Go.

## Validation

### Test Instance

- **Instance**: i-0bba96a4a2a073c22 (g4dn.xlarge, Tesla T4)
- **AMI**: al2023-ami-ecs-gpu-hvm-2023.0.20260527
- **DCGM**: datacenter-gpu-manager-4-core + 4-proprietary v4.5.2
- **Workload**: nvcr.io/nvidia/k8s/cuda-sample:nbody (1M bodies)

### Results

dcgm-init console output during GPU burn:
```
dcgm-init: connected to nv-hostengine
dcgm-init: monitoring 1 GPU(s) with profiling metrics
dcgm-init: collected gpu_util=100% sm_active=0.997267 power=71.5W temp=48C
```

Socket file content (`/var/run/ecs/gpu-metrics.sock.latest`):
```json
[{"timestamp":"2026-06-08T18:07:36Z","gpu_uuid":"","gpu_utilization_pct":100,
  "memory_utilization_pct":0,"memory_used_bytes":262144000,
  "memory_total_bytes":16106127360,"power_w":71.459,"temperature_c":48,
  "sm_active":0.9972672919294017,"sm_occupancy":0.9817307632686171,
  "dram_active":0.002802949371425993}]
```

CloudWatch (namespace `ECS/GPUMetrics`):
```
GPUSMActive     = 0.9036
GPUSMOccupancy  = 0.8896
GPUDRAMActive   = 0.0029
```

## Known Issues

1. **GPU UUID returns empty** — `FieldValue_v1.String()` for the UUID field needs blob-style extraction. Functional impact: CloudWatch dimension is empty.
2. **Profiling metrics only work on full-GPU instances** (g4dn, g5, g6, p-series). vGPU instances (g6f) fail to load the profiling module.

## Comparison with ecs-init-go-bindings-poc Branch

| Aspect | ecs-init branch (modify ecs-init) | dcgm-init branch (standalone binary) |
|--------|-----------------------------------|--------------------------------------|
| ecs-init modified | Yes | No |
| Separate binary | No | Yes (dcgm-init, 8.8 MB) |
| Release coupling | Tied to ecs-init release | Independent lifecycle |
| Systemd service | Shares ecs.service | Own service unit |
| Build system | Changes to existing Koji build | New build target |
| Failure isolation | DCGM crash could affect ecs-init | Isolated — agent/ecs-init unaffected |
