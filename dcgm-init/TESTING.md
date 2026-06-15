# dcgm-init Testing Guide

## Prerequisites

- AWS CLI configured with temporary credentials
- An EC2 SSH key pair registered in the target region
- Go 1.25+ installed (for building)
- GCC/CGO toolchain (the DCGM Go bindings use cgo)

## Architecture

dcgm-init uses **DCGM embedded mode** — it starts an in-process nv-hostengine
directly within the dcgm-init binary. This means:

- No external `nv-hostengine` daemon is required
- No TCP port 5555 is opened
- No Unix domain socket is created
- Only `libdcgm.so.4` (from `datacenter-gpu-manager-4-cuda12`) must be installed
- dcgm-init itself is managed as a systemd service

## Building

From the repository root:

```bash
make build-dcgm-init
```

This produces a `dcgm-init-bin` binary. The build uses
`CGO_LDFLAGS="-Wl,--unresolved-symbols=ignore-in-object-files"` because
`libdcgm.so` is loaded at runtime via `dlopen`, not linked at build time.

## Running Unit Tests

```bash
make test-dcgm-init
```

All tests pass without a GPU or DCGM installed.

## End-to-End Testing on a GPU Instance

### Step-by-step (manual)

1. **Build the binary** (on a matching OS, e.g., AL2023):
   ```bash
   make build-dcgm-init
   ```

2. **Launch a GPU instance** with the ECS-Optimized AL2023 GPU AMI:
   ```bash
   AMI_ID=$(aws ssm get-parameters \
       --names /aws/service/ecs/optimized-ami/amazon-linux-2023/gpu/recommended/image_id \
       --region us-west-2 \
       --query "Parameters[0].Value" --output text)

   INSTANCE_ID=$(aws ec2 run-instances \
       --image-id "$AMI_ID" \
       --instance-type g4dn.xlarge \
       --key-name <your-key-name> \
       --query "Instances[0].InstanceId" --output text)

   aws ec2 wait instance-running --instance-ids "$INSTANCE_ID"
   PUBLIC_IP=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
       --query "Reservations[0].Instances[0].PublicIpAddress" --output text)
   ```

3. **Install DCGM 4.x library** (no need to start a DCGM service):
   ```bash
   ssh ec2-user@$PUBLIC_IP
   sudo dnf install -y datacenter-gpu-manager-4-cuda12
   ```

4. **Copy and run dcgm-init** (one-shot mode):
   ```bash
   scp dcgm-init-bin ec2-user@$PUBLIC_IP:/tmp/dcgm-init
   ssh ec2-user@$PUBLIC_IP

   chmod +x /tmp/dcgm-init
   sudo mkdir -p /var/run/ecs
   sudo /tmp/dcgm-init --once --output /var/run/ecs/gpu-metrics.json
   cat /var/run/ecs/gpu-metrics.json
   ```

5. **Verify output** — you should see JSON like:
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

6. **Run as systemd service** (daemon mode):
   ```bash
   sudo cp /tmp/dcgm-init /usr/libexec/dcgm-init
   sudo cp dcgm-init.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl start dcgm-init
   sudo systemctl status dcgm-init
   ```

7. **Terminate the instance**:
   ```bash
   aws ec2 terminate-instances --instance-ids "$INSTANCE_ID"
   ```

### Automated E2E script

```bash
./scripts/test-dcgm-init-e2e.sh --key-name <your-key-name> --region us-west-2
```

## Daemon Mode

dcgm-init runs as a systemd service that writes metrics every 10 seconds:

```ini
[Unit]
Description=Amazon ECS DCGM GPU Metrics Collector
After=network.target docker.service
ConditionPathExists=/dev/nvidia0

[Service]
Type=simple
ExecStart=/usr/libexec/dcgm-init --interval 10s --output /var/run/ecs/gpu-metrics.json
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

The `ConditionPathExists=/dev/nvidia0` ensures the service only starts on GPU instances.

## Troubleshooting

- **"libdcgm.so.4 not found"**: Install DCGM 4.x:
  `sudo dnf install -y datacenter-gpu-manager-4-cuda12`
- **"failed to initialize DCGM embedded mode"**: Ensure NVIDIA drivers are loaded:
  `nvidia-smi` should work. Check `lsmod | grep nvidia`.
- **glibc version mismatch**: Build the binary on the target OS (AL2023).
  Don't build on a newer glibc host and copy to an older OS.
