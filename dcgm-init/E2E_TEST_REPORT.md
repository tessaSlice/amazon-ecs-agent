# dcgm-init E2E Test Report

**Date:** 2026-06-15  
**Tester:** Amy Lee (amytoz)  
**AWS Account:** 332896938491  
**Region:** us-west-2

## Test Environment

| Property | Value |
|----------|-------|
| Instance Type | g4dn.xlarge |
| AMI | ami-063b5c550306641d3 (ECS-Optimized AL2023 GPU) |
| OS | Amazon Linux 2023 |
| GPU | Tesla T4 (1x) |
| DCGM Version | 4.5.2 (`datacenter-gpu-manager-4-cuda12`) |
| Go Version | 1.25.9 (used for on-instance build) |
| DCGM Mode | **Embedded** (in-process nv-hostengine, no external daemon) |

## Architecture

dcgm-init uses **embedded mode** (`dcgm.Init(dcgm.Embedded)`), which starts an
in-process nv-hostengine. This means:

- **No external nv-hostengine daemon needed** — no `systemctl start nvidia-dcgm`
- **No TCP port 5555** — no network sockets opened
- **No Unix domain socket** — no `/run/nvidia-dcgm/nv-hostengine`
- **Only requires `libdcgm.so.4`** — the DCGM shared library must be installed

The dcgm-init process itself is managed by systemd via `dcgm-init.service`.

## Test Steps

### 1. Launch AL2023 GPU Instance

```bash
aws ec2 run-instances \
    --image-id ami-063b5c550306641d3 \
    --instance-type g4dn.xlarge \
    --key-name dcgm-init-e2e-test \
    --security-group-ids sg-0733c3ee1a9f71855 \
    --associate-public-ip-address \
    --region us-west-2
```

Instance ID: `i-033f1146ded22dc06`

### 2. Install DCGM 4.x Library

```bash
sudo dnf install -y datacenter-gpu-manager-4-cuda12
```

Installed: `datacenter-gpu-manager-4-core-1:4.5.2-1.x86_64`

**Note:** Only the library (`libdcgm.so.4`) is needed. The `nvidia-dcgm` service
does NOT need to be started — dcgm-init runs DCGM in embedded mode.

### 3. Build dcgm-init on Instance

```bash
cd /tmp && tar xzf dcgm-init-src.tar.gz && cd dcgm-init
CGO_ENABLED=1 CGO_LDFLAGS="-Wl,--unresolved-symbols=ignore-in-object-files" \
    go build -mod=vendor -ldflags "-s" -o /tmp/dcgm-init-bin .
```

### 4. Verify No External nv-hostengine Running

```
$ pgrep -a nv-hostengine
No nv-hostengine process found (expected for embedded mode)
```

### 5. Run dcgm-init (One-Shot Mode)

```bash
sudo /tmp/dcgm-init-bin --once --output /var/run/ecs/gpu-metrics.json
```

### 6. Run dcgm-init as systemd Service (Daemon Mode)

```bash
sudo cp /tmp/dcgm-init-bin /usr/libexec/dcgm-init
sudo cp dcgm-init.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start dcgm-init
```

Service started successfully and produced metrics within 1 second.

## Test Results: PASSED

### JSON Output (`/var/run/ecs/gpu-metrics.json`)

```json
{
  "timestamp": "2026-06-15T23:00:14Z",
  "gpus": [
    {
      "gpu_uuid": "GPU-b6fe3a59-390d-34c7-ca83-0b111557cc41",
      "gpu_utilization_percent": 0,
      "memory_utilization_percent": 0,
      "memory_total_bytes": 16106127360,
      "memory_used_bytes": 0,
      "power_draw_watts": 9.815,
      "temperature_celsius": 34,
      "restart_app_xid_count": 0
    }
  ],
  "healthy": true
}
```

### Validation Checks

| Check | Result |
|-------|--------|
| JSON parses correctly | PASS |
| `timestamp` field present (RFC3339) | PASS |
| `gpus` array present and non-empty | PASS |
| `healthy` field is `true` | PASS |
| GPU UUID starts with `GPU-` | PASS |
| `memory_total_bytes` > 0 (15.0 GiB) | PASS |
| `temperature_celsius` > 0 (34°C) | PASS |
| `power_draw_watts` > 0 (9.815W) | PASS |
| `gpu_utilization_percent` is numeric (0%) | PASS |
| `restart_app_xid_count` is 0 (no errors) | PASS |
| No external nv-hostengine running | PASS |
| No TCP port 5555 opened | PASS |
| systemd service runs and produces metrics | PASS |

### Key Log Output (Embedded Mode)

```
initializing DCGM client in embedded mode
DCGM embedded mode initialized successfully
successfully registered policy violation listeners
successfully enabled health check systems
persistent metrics field watches enabled successfully (fieldCount: 7)
XID error field watch enabled successfully
DCGM client initialized successfully (metricsWatchActive: true, xidWatchActive: true)
metrics written (path: /var/run/ecs/gpu-metrics.json, gpuCount: 1)
DCGM client shutdown successfully
```

### systemd Service Status

```
● dcgm-init.service - Amazon ECS DCGM GPU Metrics Collector
     Loaded: loaded (/etc/systemd/system/dcgm-init.service; disabled; preset: disabled)
     Active: active (running)
   Main PID: 12156 (dcgm-init)
      Tasks: 23
     Memory: 23.7M
```

## Unit Tests

All unit tests pass without GPU hardware:

```
$ make test-dcgm-init
ok  github.com/aws/amazon-ecs-agent/dcgm-init/gpu    0.057s
```

## Cleanup

All AWS resources terminated after testing:
- Instance `i-033f1146ded22dc06`: terminated
- Key pair `dcgm-init-e2e-test`: deleted
- Security group `sg-0733c3ee1a9f71855`: pending deletion
