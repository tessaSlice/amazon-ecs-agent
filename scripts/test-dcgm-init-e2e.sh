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
REGION="${AWS_DEFAULT_REGION:-us-east-1}"
INSTANCE_TYPE="g4dn.xlarge"
SECURITY_GROUP=""
SUBNET_ID=""
INSTANCE_ID=""
PUBLIC_IP=""
BINARY_PATH="./dcgm-init-bin"

usage() {
    echo "Usage: $0 --key-name <ec2-key-pair-name> [--region <aws-region>] [--instance-type <type>] [--security-group <sg-id>] [--subnet <subnet-id>]"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --key-name) KEY_NAME="$2"; shift 2 ;;
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

cleanup() {
    if [[ -n "$INSTANCE_ID" ]]; then
        echo "Terminating instance $INSTANCE_ID..."
        aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region "$REGION" 2>/dev/null || true
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

echo "Waiting for SSH to become available..."
for i in $(seq 1 30); do
    if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 ec2-user@"$PUBLIC_IP" true 2>/dev/null; then
        break
    fi
    echo "  Attempt $i/30..."
    sleep 10
done

echo "=== Step 3: Install DCGM on the instance ==="
ssh -o StrictHostKeyChecking=no ec2-user@"$PUBLIC_IP" << 'INSTALL_DCGM'
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
scp -o StrictHostKeyChecking=no "$BINARY_PATH" ec2-user@"$PUBLIC_IP":/tmp/dcgm-init

ssh -o StrictHostKeyChecking=no ec2-user@"$PUBLIC_IP" << 'RUN_DCGM_INIT'
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
