# dcgm-init E2E Testing Guide (From Scratch)

This guide walks through running the dcgm-init end-to-end test starting from
nothing but temporary AWS credentials. Every resource (key pair, security group,
instance) is created and torn down within the test.

## Prerequisites

- AWS CLI installed and configured with temporary credentials (`aws sts get-caller-identity` works)
- Go 1.25+ installed locally (for building)
- GCC installed locally (cgo requirement)
- The repository checked out with the `dcgm-init/` directory

## Step 1: Build the binary

```bash
make build-dcgm-init
```

This produces `./amazon-dcgm-init` in the repo root.

## Step 2: Create an EC2 key pair

```bash
REGION=us-west-2

aws ec2 create-key-pair \
    --key-name dcgm-init-e2e-test \
    --region $REGION \
    --query "KeyMaterial" \
    --output text > ~/.ssh/dcgm-init-e2e-test.pem

chmod 600 ~/.ssh/dcgm-init-e2e-test.pem
```

## Step 3: Create a security group allowing SSH

```bash
VPC_ID=$(aws ec2 describe-vpcs \
    --filters Name=isDefault,Values=true \
    --region $REGION \
    --query 'Vpcs[0].VpcId' --output text)

SG_ID=$(aws ec2 create-security-group \
    --group-name dcgm-init-e2e-sg \
    --description "SSH access for dcgm-init E2E test" \
    --vpc-id "$VPC_ID" \
    --region $REGION \
    --query 'GroupId' --output text)

aws ec2 authorize-security-group-ingress \
    --group-id "$SG_ID" \
    --protocol tcp --port 22 --cidr 0.0.0.0/0 \
    --region $REGION
```

> **Note:** Using `0.0.0.0/0` is acceptable for a short-lived test instance.
> If your corporate network rotates egress IPs, restricting to a single `/32`
> may cause SSH timeouts.

## Step 4: Find the ECS-Optimized AL2023 GPU AMI

```bash
AMI_ID=$(aws ssm get-parameters \
    --names /aws/service/ecs/optimized-ami/amazon-linux-2023/gpu/recommended/image_id \
    --region $REGION \
    --query "Parameters[0].Value" --output text)

echo "AMI: $AMI_ID"
```

## Step 5: Launch a GPU instance

```bash
INSTANCE_ID=$(aws ec2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type g4dn.xlarge \
    --key-name dcgm-init-e2e-test \
    --security-group-ids "$SG_ID" \
    --associate-public-ip-address \
    --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=dcgm-init-e2e-test}]' \
    --region $REGION \
    --query "Instances[0].InstanceId" --output text)

echo "Instance: $INSTANCE_ID"

aws ec2 wait instance-running --instance-ids "$INSTANCE_ID" --region $REGION

PUBLIC_IP=$(aws ec2 describe-instances \
    --instance-ids "$INSTANCE_ID" \
    --region $REGION \
    --query "Reservations[0].Instances[0].PublicIpAddress" --output text)

echo "IP: $PUBLIC_IP"
```

## Step 6: Wait for SSH

```bash
for i in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
        -i ~/.ssh/dcgm-init-e2e-test.pem ec2-user@$PUBLIC_IP "echo OK" 2>/dev/null; then
        echo "SSH ready"; break
    fi
    echo "Waiting... ($i/30)"; sleep 10
done
```

## Step 7: Install DCGM 4.x on the instance

```bash
ssh -o StrictHostKeyChecking=no -o BatchMode=yes \
    -i ~/.ssh/dcgm-init-e2e-test.pem ec2-user@$PUBLIC_IP \
    "sudo dnf install -y datacenter-gpu-manager-4-cuda12"
```

This installs `libdcgm.so.4` which dcgm-init loads at runtime. No DCGM service
needs to be started — dcgm-init uses embedded mode.

## Step 8: Copy source, build, and run on the instance

The binary must be built on the instance (or a matching AL2023 host) to avoid
glibc version mismatches.

```bash
# Copy source
tar czf /tmp/dcgm-init-src.tar.gz dcgm-init/
scp -o StrictHostKeyChecking=no -o BatchMode=yes \
    -i ~/.ssh/dcgm-init-e2e-test.pem \
    /tmp/dcgm-init-src.tar.gz ec2-user@$PUBLIC_IP:/tmp/

# Build and run on the instance
ssh -o StrictHostKeyChecking=no -o BatchMode=yes \
    -i ~/.ssh/dcgm-init-e2e-test.pem ec2-user@$PUBLIC_IP << 'EOF'
# Install Go
curl -sL https://go.dev/dl/go1.25.9.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
export PATH=$PATH:/usr/local/go/bin

# Build
cd /tmp && tar xzf dcgm-init-src.tar.gz && cd dcgm-init
CGO_ENABLED=1 CGO_LDFLAGS="-Wl,--unresolved-symbols=ignore-in-object-files" \
    go build -mod=vendor -ldflags "-s" -o /tmp/amazon-dcgm-init .

# Run (one-shot, embedded mode — no nv-hostengine needed)
sudo mkdir -p /var/run/ecs
sudo /tmp/amazon-dcgm-init --once --output /var/run/ecs/gpu-metrics.json

# Show output
cat /var/run/ecs/gpu-metrics.json
EOF
```

## Step 9: Validate the output

```bash
ssh -o StrictHostKeyChecking=no -o BatchMode=yes \
    -i ~/.ssh/dcgm-init-e2e-test.pem ec2-user@$PUBLIC_IP \
    python3 -c "
import json
with open('/var/run/ecs/gpu-metrics.json') as f:
    data = json.load(f)
assert data['healthy'] == True
assert len(data['gpus']) >= 1
gpu = data['gpus'][0]
assert gpu['gpu_uuid'].startswith('GPU-')
assert gpu['memory_total_bytes'] > 0
assert gpu['temperature_celsius'] > 0
assert gpu['power_draw_watts'] > 0
print('ALL VALIDATIONS PASSED')
print(f'  GPU: {gpu[\"gpu_uuid\"]}')
print(f'  Memory: {gpu[\"memory_total_bytes\"] / (1024**3):.1f} GiB')
print(f'  Temp: {gpu[\"temperature_celsius\"]}C, Power: {gpu[\"power_draw_watts\"]}W')
"
```

Expected output:
```
ALL VALIDATIONS PASSED
  GPU: GPU-57214b1c-d34c-a8b8-d631-4bb056a48da0
  Memory: 15.0 GiB
  Temp: 32C, Power: 9.414W
```

## Step 10: Clean up

```bash
# Terminate instance
aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region $REGION

# Wait for full termination (needed before SG can be deleted)
aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" --region $REGION

# Delete security group
aws ec2 delete-security-group --group-id "$SG_ID" --region $REGION

# Delete key pair
aws ec2 delete-key-pair --key-name dcgm-init-e2e-test --region $REGION
rm -f ~/.ssh/dcgm-init-e2e-test.pem
```

## Automated script

The above steps are automated in `scripts/test-dcgm-init-e2e.sh`:

```bash
./scripts/test-dcgm-init-e2e.sh \
    --key-name dcgm-init-e2e-test \
    --key-file ~/.ssh/dcgm-init-e2e-test.pem \
    --region us-west-2
```

The script handles security group creation, instance launch, SSH retry, DCGM
install, build, run, validation, and cleanup automatically. Pass
`--security-group <sg-id>` if you already have one with SSH access.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| SSH times out after 30 attempts | Security group doesn't allow your IP | Use `0.0.0.0/0` CIDR or pass a pre-created `--security-group` |
| `InsufficientInstanceCapacity` | AZ has no g4dn capacity | Try a different subnet/AZ or use `--instance-type g5.xlarge` |
| `GLIBC_2.32 not found` | Binary built on newer glibc host | Build on the instance itself (as shown above) |
| `libdcgm.so.4 not found` | DCGM 4.x not installed | Run `sudo dnf install -y datacenter-gpu-manager-4-cuda12` |
| `failed to initialize DCGM embedded mode` | NVIDIA drivers not loaded | Run `nvidia-smi` to verify drivers work |

## Cost

A g4dn.xlarge costs ~$0.526/hr. The full test takes ~5 minutes, so each run
costs approximately $0.04.
