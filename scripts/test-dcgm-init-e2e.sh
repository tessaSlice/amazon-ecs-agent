#!/bin/bash
# Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License"). You may
# not use this file except in compliance with the License. A copy of the
# License is located at
#
#     http://aws.amazon.com/apache2.0/
#
# or in the "license" file accompanying this file. This file is distributed
# on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
# express or implied. See the License for the specific language governing
# permissions and limitations under the License.

# End-to-end test for dcgm-init on a GPU EC2 instance.
# This script:
#   1. Launches a GPU EC2 instance with ECS-Optimized GPU AMI
#   2. Installs datacenter-gpu-manager-4-core (DCGM)
#   3. Copies and runs dcgm-init on the instance
#   4. Verifies JSON metrics output
#
# Prerequisites:
#   - AWS CLI configured with temporary credentials
#   - An SSH key pair registered in EC2
#   - The dcgm-init binary built (run: make build-dcgm-init)
#
# Usage:
#   ./scripts/test-dcgm-init-e2e.sh [--key-name <key>] [--region <region>]
#
set -euo pipefail

KEY_NAME="${KEY_NAME:-}"
KEY_FILE="${KEY_FILE:-}"
REGION="${AWS_DEFAULT_REGION:-us-east-1}"
INSTANCE_TYPE="g4dn.xlarge"
SECURITY_GROUP=""
CREATED_SG=""
SUBNET_ID=""
INSTANCE_ID=""
PUBLIC_IP=""
BINARY_PATH="./dcgm-init-bin"
# Static, predictable env file name (independent of the SSH key name).
ENV_FILE="dcgm-init-verify.env"

usage() {
    echo "Usage: $0 --key-name <ec2-key-pair-name> [--key-file <path-to-private-key.pem>] [--region <aws-region>] [--instance-type <type>] [--security-group <sg-id>] [--subnet <subnet-id>]"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --key-name) KEY_NAME="$2"; shift 2 ;;
        --key-file) KEY_FILE="$2"; shift 2 ;;
        --region) REGION="$2"; shift 2 ;;
        --instance-type) INSTANCE_TYPE="$2"; shift 2 ;;
        --security-group) SECURITY_GROUP="$2"; shift 2 ;;
        --subnet) SUBNET_ID="$2"; shift 2 ;;
        *) usage ;;
    esac
done

if [[ -z "$KEY_NAME" ]]; then
    echo "ERROR: --key-name is required"
    usage
fi

if [[ ! -f "$BINARY_PATH" ]]; then
    echo "ERROR: dcgm-init binary not found at $BINARY_PATH"
    echo "Run 'make build-dcgm-init' first."
    exit 1
fi

# An EC2 key pair name (--key-name) only registers the *public* key with AWS.
# ssh/scp still need the matching *private* key locally. Default to the common
# ~/.ssh/<key-name>.pem location when --key-file isn't supplied.
if [[ -z "$KEY_FILE" ]]; then
    KEY_FILE="${HOME}/.ssh/${KEY_NAME}.pem"
fi
if [[ ! -f "$KEY_FILE" ]]; then
    echo "ERROR: SSH private key not found at '$KEY_FILE'."
    echo "Pass --key-file <path-to-private-key.pem> for EC2 key pair '$KEY_NAME'."
    exit 1
fi
chmod 600 "$KEY_FILE" 2>/dev/null || true

# Shared SSH options used everywhere. The important ones for avoiding hangs:
#   -i "$KEY_FILE"     use the correct private key (otherwise auth fails)
#   BatchMode=yes      never block on an interactive password/passphrase prompt
#   ConnectTimeout=10  fail a stalled connection fast instead of waiting forever
SSH_OPTS=(-i "$KEY_FILE" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o BatchMode=yes -o ConnectTimeout=10)

cleanup() {
    if [[ -n "$INSTANCE_ID" ]]; then
        echo "Terminating instance $INSTANCE_ID..."
        aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region "$REGION" >/dev/null 2>&1 || true
        # Wait for termination so the security group is no longer in use.
        aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" --region "$REGION" 2>/dev/null || true
    fi
    if [[ -n "$CREATED_SG" ]]; then
        echo "Deleting security group $CREATED_SG..."
        aws ec2 delete-security-group --group-id "$CREATED_SG" --region "$REGION" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo "=== Step 1: Find ECS-Optimized GPU AMI (AL2023) ==="
AMI_ID=$(aws ssm get-parameters \
    --names /aws/service/ecs/optimized-ami/amazon-linux-2023/gpu/recommended/image_id \
    --region "$REGION" \
    --query "Parameters[0].Value" \
    --output text)
echo "Using AMI: $AMI_ID"

echo "=== Step 1b: Ensure a security group that allows inbound SSH ==="
# Without an inbound TCP 22 rule, every SSH attempt times out and the script
# appears to hang at "Waiting for SSH...". If the caller didn't supply a
# security group, create a temporary one that allows SSH only from this host.
if [[ -z "$SECURITY_GROUP" ]]; then
    MY_IP="$(curl -fsS https://checkip.amazonaws.com)"
    if [[ -n "$SUBNET_ID" ]]; then
        VPC_ID=$(aws ec2 describe-subnets --subnet-ids "$SUBNET_ID" --region "$REGION" \
            --query 'Subnets[0].VpcId' --output text)
    else
        VPC_ID=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true --region "$REGION" \
            --query 'Vpcs[0].VpcId' --output text)
    fi
    SECURITY_GROUP=$(aws ec2 create-security-group --group-name "dcgm-init-e2e-$$" \
        --description "Temporary SSH access for dcgm-init e2e test" \
        --vpc-id "$VPC_ID" --region "$REGION" --query 'GroupId' --output text)
    CREATED_SG="$SECURITY_GROUP"
    aws ec2 authorize-security-group-ingress --group-id "$SECURITY_GROUP" \
        --protocol tcp --port 22 --cidr "${MY_IP}/32" --region "$REGION" >/dev/null
    echo "Created temporary security group $SECURITY_GROUP (SSH from ${MY_IP}/32)"
else
    echo "Using provided security group $SECURITY_GROUP (ensure it allows inbound TCP 22)"
fi

echo "=== Step 2: Launch GPU EC2 Instance ==="
RUN_ARGS="--image-id $AMI_ID --instance-type $INSTANCE_TYPE --key-name $KEY_NAME --region $REGION"
RUN_ARGS="$RUN_ARGS --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=dcgm-init-e2e-test}]'"

if [[ -n "$SECURITY_GROUP" ]]; then
    RUN_ARGS="$RUN_ARGS --security-group-ids $SECURITY_GROUP"
fi
if [[ -n "$SUBNET_ID" ]]; then
    RUN_ARGS="$RUN_ARGS --subnet-id $SUBNET_ID"
fi

INSTANCE_ID=$(eval aws ec2 run-instances $RUN_ARGS \
    --query "Instances[0].InstanceId" \
    --output text)
echo "Launched instance: $INSTANCE_ID"

echo "Waiting for instance to be running..."
aws ec2 wait instance-running --instance-ids "$INSTANCE_ID" --region "$REGION"

PUBLIC_IP=$(aws ec2 describe-instances \
    --instance-ids "$INSTANCE_ID" \
    --region "$REGION" \
    --query "Reservations[0].Instances[0].PublicIpAddress" \
    --output text)
echo "Instance public IP: $PUBLIC_IP"
if [[ -z "$PUBLIC_IP" || "$PUBLIC_IP" == "None" ]]; then
    echo "ERROR: instance has no public IP. Use a subnet that auto-assigns public IPs." >&2
    exit 1
fi

# Save connection details to a stable, predictable env file so it can be sourced
# later (e.g. to ssh in or fetch metrics). The name never changes.
cat > "$ENV_FILE" <<EOF
# dcgm-init e2e connection details for ${INSTANCE_ID}.
# Usage: source ${ENV_FILE}; ssh -i "\$KEY_FILE" "\$SSH_USER@\$PUBLIC_IP"
export REGION=${REGION}
export INSTANCE_ID=${INSTANCE_ID}
export PUBLIC_IP=${PUBLIC_IP}
export KEY_NAME=${KEY_NAME}
export KEY_FILE=${KEY_FILE}
export SSH_USER=ec2-user
EOF
echo "Saved connection details to ${ENV_FILE}"

echo "Waiting for SSH to become available..."
SSH_OK=false
for i in $(seq 1 30); do
    if ssh "${SSH_OPTS[@]}" ec2-user@"$PUBLIC_IP" true 2>/dev/null; then
        SSH_OK=true
        break
    fi
    echo "  Attempt $i/30..."
    sleep 10
done
if [[ "$SSH_OK" != true ]]; then
    echo "ERROR: SSH never became available after ~5 minutes." >&2
    echo "Check that: (1) the security group allows inbound TCP 22 from your IP," >&2
    echo "            (2) --key-file '$KEY_FILE' matches EC2 key pair '$KEY_NAME'." >&2
    exit 1
fi

echo "=== Step 3: Install DCGM on the instance ==="
ssh "${SSH_OPTS[@]}" ec2-user@"$PUBLIC_IP" << 'INSTALL_DCGM'
# Install DCGM 4.x (required by go-dcgm bindings)
if command -v dnf &>/dev/null; then
    sudo dnf install -y datacenter-gpu-manager-4-cuda12
else
    sudo yum install -y datacenter-gpu-manager-4-core
fi
# Start nv-hostengine with Unix domain socket
sudo mkdir -p /run/nvidia-dcgm
sudo nv-hostengine -n -d /run/nvidia-dcgm/nv-hostengine &
sleep 5
# Verify DCGM is running
ls -la /run/nvidia-dcgm/nv-hostengine
dcgmi discovery -l
INSTALL_DCGM

echo "=== Step 4: Copy and run dcgm-init ==="
scp "${SSH_OPTS[@]}" "$BINARY_PATH" ec2-user@"$PUBLIC_IP":/tmp/dcgm-init

ssh "${SSH_OPTS[@]}" ec2-user@"$PUBLIC_IP" << 'RUN_DCGM_INIT'
chmod +x /tmp/dcgm-init
sudo mkdir -p /var/run/ecs
sudo /tmp/dcgm-init --once --output /var/run/ecs/gpu-metrics.json
echo ""
echo "=== GPU Metrics Output ==="
cat /var/run/ecs/gpu-metrics.json
echo ""
echo "=== Validating JSON structure ==="
python3 -c "
import json, sys
with open('/var/run/ecs/gpu-metrics.json') as f:
    data = json.load(f)
assert 'timestamp' in data, 'Missing timestamp'
assert 'gpus' in data, 'Missing gpus array'
assert 'healthy' in data, 'Missing healthy field'
assert len(data['gpus']) > 0, 'No GPU metrics found'
for gpu in data['gpus']:
    assert 'gpu_uuid' in gpu, f'Missing gpu_uuid'
    assert gpu['gpu_uuid'].startswith('GPU-'), f'Invalid UUID format: {gpu[\"gpu_uuid\"]}'
print(f'SUCCESS: Found {len(data[\"gpus\"])} GPU(s) with valid metrics')
print(f'  Healthy: {data[\"healthy\"]}')
for i, gpu in enumerate(data['gpus']):
    print(f'  GPU {i}: UUID={gpu[\"gpu_uuid\"]}')
    if gpu.get('gpu_utilization_percent') is not None:
        print(f'    Utilization: {gpu[\"gpu_utilization_percent\"]}%')
    if gpu.get('temperature_celsius') is not None:
        print(f'    Temperature: {gpu[\"temperature_celsius\"]}°C')
    if gpu.get('memory_total_bytes') is not None:
        print(f'    Memory: {gpu[\"memory_used_bytes\"]}/{gpu[\"memory_total_bytes\"]} bytes')
"
RUN_DCGM_INIT

echo ""
echo "=== E2E Test PASSED ==="
echo "Instance $INSTANCE_ID will be terminated on exit."
