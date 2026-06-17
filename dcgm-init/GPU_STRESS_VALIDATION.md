# dcgm-init GPU Stress Validation Procedure

This document describes how to validate that dcgm-init reports real-time GPU
metrics under load on an AL2023 ECS-Optimized GPU instance.

## Prerequisites

- AWS CLI configured with valid credentials
- SSH key pair registered in EC2

## Procedure

### 1. Launch a GPU instance

```bash
REGION=us-west-2
KEY_NAME=<your-key-name>

AMI_ID=$(aws ssm get-parameters \
    --names /aws/service/ecs/optimized-ami/amazon-linux-2023/gpu/recommended/image_id \
    --region $REGION --query "Parameters[0].Value" --output text)

# Create a security group allowing SSH
VPC_ID=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true \
    --region $REGION --query 'Vpcs[0].VpcId' --output text)
SG_ID=$(aws ec2 create-security-group --group-name "dcgm-stress-test" \
    --description "SSH for stress test" --vpc-id "$VPC_ID" \
    --region $REGION --query 'GroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
    --protocol tcp --port 22 --cidr 0.0.0.0/0 --region $REGION

# Launch g4dn.12xlarge (4x Tesla T4)
INSTANCE_ID=$(aws ec2 run-instances \
    --image-id "$AMI_ID" --instance-type g4dn.12xlarge \
    --key-name "$KEY_NAME" --security-group-ids "$SG_ID" \
    --associate-public-ip-address \
    --region $REGION --query "Instances[0].InstanceId" --output text)

aws ec2 wait instance-running --instance-ids "$INSTANCE_ID" --region $REGION
PUBLIC_IP=$(aws ec2 describe-instances --instance-ids "$INSTANCE_ID" \
    --region $REGION \
    --query "Reservations[0].Instances[0].PublicIpAddress" --output text)
echo "SSH: ssh -i ~/.ssh/${KEY_NAME}.pem ec2-user@${PUBLIC_IP}"
```

### 2. Install DCGM and CUDA toolkit

```bash
ssh -i ~/.ssh/${KEY_NAME}.pem ec2-user@${PUBLIC_IP}

# On the instance:
sudo dnf install -y datacenter-gpu-manager-4-cuda12 cuda-toolkit-12-6
```

### 3. Build dcgm-init

Copy the dcgm-init source to the instance and build:

```bash
# From your local machine:
tar czf /tmp/dcgm-init-src.tar.gz dcgm-init/
scp -i ~/.ssh/${KEY_NAME}.pem /tmp/dcgm-init-src.tar.gz ec2-user@${PUBLIC_IP}:/tmp/

# On the instance:
curl -sL https://go.dev/dl/go1.25.9.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
export PATH=$PATH:/usr/local/go/bin
cd /tmp && tar xzf dcgm-init-src.tar.gz && cd dcgm-init
CGO_ENABLED=1 CGO_LDFLAGS="-Wl,--unresolved-symbols=ignore-in-object-files" \
    go build -mod=vendor -ldflags "-s" -o /tmp/amazon-dcgm-init .
```

### 4. Create the GPU stress test program

```bash
# On the instance:
cat > /tmp/gpu_stress.cu << 'CUDA'
#include <stdio.h>
#include <cuda_runtime.h>

__global__ void stress_kernel(float *data, int n) {
    int idx = blockIdx.x * blockDim.x + threadIdx.x;
    if (idx < n) {
        float val = data[idx];
        for (int i = 0; i < 1000; i++) {
            val = val * 1.00001f + 0.00001f;
        }
        data[idx] = val;
    }
}

int main() {
    int deviceCount;
    cudaGetDeviceCount(&deviceCount);
    printf("Found %d GPUs, stressing all for 300 seconds...\n", deviceCount);

    const int N = 50000000;
    float **d_data = (float**)malloc(deviceCount * sizeof(float*));

    for (int d = 0; d < deviceCount; d++) {
        cudaSetDevice(d);
        cudaMalloc(&d_data[d], N * sizeof(float));
        cudaMemset(d_data[d], 0, N * sizeof(float));
    }

    int blocks = (N + 255) / 256;
    time_t start = time(NULL);
    while (time(NULL) - start < 300) {
        for (int d = 0; d < deviceCount; d++) {
            cudaSetDevice(d);
            stress_kernel<<<blocks, 256>>>(d_data[d], N);
        }
        for (int d = 0; d < deviceCount; d++) {
            cudaSetDevice(d);
            cudaDeviceSynchronize();
        }
    }

    for (int d = 0; d < deviceCount; d++) {
        cudaSetDevice(d);
        cudaFree(d_data[d]);
    }
    free(d_data);
    printf("Stress complete.\n");
    return 0;
}
CUDA

/usr/local/cuda-12.6/bin/nvcc -o /tmp/gpu_stress /tmp/gpu_stress.cu
```

### 5. Start dcgm-init as a systemd service

```bash
# On the instance:
sudo cp /tmp/amazon-dcgm-init /usr/libexec/dcgm-init

sudo tee /etc/systemd/system/dcgm-init.service << 'UNIT'
[Unit]
Description=Amazon ECS DCGM GPU Metrics Collector
After=network.target docker.service
ConditionPathExists=/dev/nvidia0

[Service]
Type=simple
ExecStart=/usr/libexec/dcgm-init --interval 60s --output /var/run/ecs/gpu-metrics.json
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT

sudo mkdir -p /var/run/ecs
sudo systemctl daemon-reload
sudo systemctl start dcgm-init
```

### 6. Observe idle metrics

```bash
cat /var/run/ecs/gpu-metrics.json
```

Expected: ~9W power, ~33°C temperature, 0% utilization.

### 7. Start GPU stress and observe metrics change

```bash
# Start stress in background
export LD_LIBRARY_PATH=/usr/local/cuda-12.6/lib64:$LD_LIBRARY_PATH
/tmp/gpu_stress &

# Watch metrics update (next write in up to 60s)
watch -d cat /var/run/ecs/gpu-metrics.json
```

Expected after next collection tick: ~70W power, ~50°C+ temperature, 90-100%
utilization.

### 8. Clean up

```bash
# From your local machine:
aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region $REGION
aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" --region $REGION
aws ec2 delete-security-group --group-id "$SG_ID" --region $REGION
```
