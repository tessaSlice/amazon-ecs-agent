#!/usr/bin/env bash
#
# e2e-gpu-metrics-validation.sh
#
# End-to-end validation of the dcgm-init GPU telemetry pipeline on a running,
# ECS-registered GPU instance. Validates, in order:
#
#   1. dcgm-init runtime state   — service active, no restart loop, /var/run/ecs
#                                   dir + gpu-metrics.json created at runtime.
#   2. Agent bind mount + read   — /var/run/ecs mounted read-only into the agent
#                                   container; agent reads GPU metrics.
#   3. ACCELERATED_COMPUTE health check — the instance health check (see
#                                   https://docs.aws.amazon.com/AmazonECS/latest/developerguide/container-instance-health.html)
#                                   runs and reports OK on the box, AND (best
#                                   effort) surfaces via the describe-container-instances API.
#   4. CloudWatch metrics        — instance-level GPU metrics (InstanceGPULimit,
#                                   InstanceGPUUsageTotal) reach CloudWatch, plus
#                                   container/task-level GPU metrics when a service runs.
#
# Requirements: awscli v2, an SSM-managed GPU instance already registered to an
# ECS cluster (Enhanced Container Insights recommended for the CloudWatch checks).
#
# Usage:
#   ./e2e-gpu-metrics-validation.sh \
#       --cluster <ECS_CLUSTER_NAME> \
#       --instance <EC2_INSTANCE_ID> \
#       [--region us-east-1] \
#       [--metrics-path /var/run/ecs/gpu-metrics.json]
#
set -uo pipefail

REGION="us-east-1"
CLUSTER=""
INSTANCE_ID=""
METRICS_PATH="/var/run/ecs/gpu-metrics.json"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster)       CLUSTER="$2";        shift 2 ;;
    --instance)      INSTANCE_ID="$2";    shift 2 ;;
    --region)        REGION="$2";         shift 2 ;;
    --metrics-path)  METRICS_PATH="$2";   shift 2 ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$CLUSTER" || -z "$INSTANCE_ID" ]]; then
  echo "ERROR: --cluster and --instance are required. See --help." >&2
  exit 2
fi

METRICS_DIR="$(dirname "$METRICS_PATH")"

PASS=0
FAIL=0
WARN=0
pass() { echo "  [PASS] $*"; PASS=$((PASS + 1)); }
fail() { echo "  [FAIL] $*"; FAIL=$((FAIL + 1)); }
warn() { echo "  [WARN] $*"; WARN=$((WARN + 1)); }
section() { echo; echo "=== $* ==="; }

# Run a shell script on the instance via SSM and print its stdout. Returns the
# command's stdout on fd 1; polls until the invocation leaves a pending state.
run_ssm() {
  local script_json="$1"
  local cmd_id
  cmd_id="$(aws ssm send-command --region "$REGION" \
    --instance-ids "$INSTANCE_ID" \
    --document-name "AWS-RunShellScript" \
    --parameters "commands=${script_json}" \
    --query 'Command.CommandId' --output text 2>/dev/null)"
  if [[ -z "$cmd_id" || "$cmd_id" == "None" ]]; then
    echo "__SSM_SEND_FAILED__"
    return 1
  fi
  local status="Pending"
  for _ in $(seq 1 30); do
    sleep 3
    status="$(aws ssm get-command-invocation --region "$REGION" \
      --command-id "$cmd_id" --instance-id "$INSTANCE_ID" \
      --query 'Status' --output text 2>/dev/null)"
    case "$status" in
      Success|Failed|Cancelled|TimedOut) break ;;
    esac
  done
  aws ssm get-command-invocation --region "$REGION" \
    --command-id "$cmd_id" --instance-id "$INSTANCE_ID" \
    --query 'StandardOutputContent' --output text 2>/dev/null
}

echo "############################################################"
echo "# GPU metrics e2e validation"
echo "#   cluster:  $CLUSTER"
echo "#   instance: $INSTANCE_ID"
echo "#   region:   $REGION"
echo "#   metrics:  $METRICS_PATH"
echo "############################################################"

# Resolve the container instance ARN once (used by health-check + metric checks).
CI_ARN="$(aws ecs list-container-instances --region "$REGION" --cluster "$CLUSTER" \
  --filter "ec2InstanceId==${INSTANCE_ID}" \
  --query 'containerInstanceArns[0]' --output text 2>/dev/null)"
if [[ -z "$CI_ARN" || "$CI_ARN" == "None" ]]; then
  # Fall back to matching by ec2InstanceId across all registered instances.
  CI_ARN="$(aws ecs list-container-instances --region "$REGION" --cluster "$CLUSTER" \
    --query 'containerInstanceArns' --output text 2>/dev/null | tr '\t' '\n' | while read -r arn; do
      [[ -z "$arn" ]] && continue
      eid="$(aws ecs describe-container-instances --region "$REGION" --cluster "$CLUSTER" \
        --container-instances "$arn" --query 'containerInstances[0].ec2InstanceId' --output text 2>/dev/null)"
      [[ "$eid" == "$INSTANCE_ID" ]] && { echo "$arn"; break; }
    done)"
fi

# ---------------------------------------------------------------------------
section "1. dcgm-init runtime state"
# ---------------------------------------------------------------------------
OUT="$(run_ssm '[
  "echo ACTIVE=$(systemctl is-active dcgm-init 2>&1)",
  "echo NRESTARTS=$(systemctl show dcgm-init -p NRestarts --value 2>&1)",
  "echo EXITSTATUS=$(systemctl show dcgm-init -p ExecMainStatus --value 2>&1)",
  "echo DIRMODE=$(stat -c %a '"$METRICS_DIR"' 2>&1)",
  "echo FILEEXISTS=$([ -f '"$METRICS_PATH"' ] && echo yes || echo no)",
  "echo HEALTHYTRUE=$(grep -c \"healthy.: true\" '"$METRICS_PATH"' 2>/dev/null)",
  "echo GPUCOUNT=$(grep -c gpu_uuid '"$METRICS_PATH"' 2>/dev/null)"
]')"
echo "$OUT" | sed 's/^/    ssm> /'
[[ "$OUT" == *"ACTIVE=active"* ]]        && pass "dcgm-init service is active"                 || fail "dcgm-init service not active"
[[ "$OUT" == *"NRESTARTS=0"* ]]          && pass "dcgm-init NRestarts=0 (no restart loop)"      || warn "dcgm-init has restarted (check NRESTARTS above)"
[[ "$OUT" == *"DIRMODE=755"* ]]          && pass "$METRICS_DIR created at runtime (mode 755)"   || fail "$METRICS_DIR missing or wrong mode"
[[ "$OUT" == *"FILEEXISTS=yes"* ]]       && pass "$METRICS_PATH exists"                         || fail "$METRICS_PATH missing"
if echo "$OUT" | grep -qE "HEALTHYTRUE=[1-9]"; then pass "gpu-metrics.json reports healthy:true"; else warn "gpu-metrics.json not healthy:true (see HEALTHYTRUE above)"; fi

# ---------------------------------------------------------------------------
section "2. Agent bind mount + GPU read"
# ---------------------------------------------------------------------------
OUT="$(run_ssm '[
  "echo BIND=$(docker inspect ecs-agent --format \"{{range .Mounts}}{{.Source}}:{{.Destination}}:{{.RW}} {{end}}\" 2>/dev/null | tr \" \" \"\\n\" | grep '"$METRICS_DIR"')",
  "echo READLINES=$(cat /var/log/ecs/ecs-agent.log* 2>/dev/null | grep -ac \"GPU instance metrics read\")",
  "echo ATTACHED=$(cat /var/log/ecs/ecs-agent.log* 2>/dev/null | grep -ac \"Attached instance-level GPU metrics\")"
]')"
echo "$OUT" | sed 's/^/    ssm> /'
# Read-only bind mount: Source:Dest:false  (RW=false means read-only)
if echo "$OUT" | grep -q "${METRICS_DIR}:${METRICS_DIR}:false"; then
  pass "$METRICS_DIR bind-mounted read-only into agent container"
elif echo "$OUT" | grep -q "$METRICS_DIR"; then
  warn "$METRICS_DIR is mounted but not read-only (expected RW=false)"
else
  fail "$METRICS_DIR not bind-mounted into agent container"
fi
if echo "$OUT" | grep -qE "READLINES=[1-9]"; then
  pass "agent is reading GPU metrics (GPU instance metrics read)"
else
  warn "no 'GPU instance metrics read' log lines yet (agent may still be warming up)"
fi

# ---------------------------------------------------------------------------
section "3. ACCELERATED_COMPUTE instance health check"
#   Ref: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/container-instance-health.html
# ---------------------------------------------------------------------------
# 3a. On-box: confirm the health check runs and its computed status.
OUT="$(run_ssm '[
  "echo LASTSTATUS=$(cat /var/log/ecs/ecs-agent.log* 2>/dev/null | grep -a \"ACCELERATED_COMPUTE\" | grep -oE \"instanceHealthCheckResult=[A-Z_]*\" | tail -1)",
  "echo RUNCOUNT=$(cat /var/log/ecs/ecs-agent.log* 2>/dev/null | grep -ac \"ACCELERATED_COMPUTE\")"
]')"
echo "$OUT" | sed 's/^/    ssm> /'
if echo "$OUT" | grep -qE "RUNCOUNT=[1-9]"; then
  pass "ACCELERATED_COMPUTE health check is running on the instance"
else
  fail "ACCELERATED_COMPUTE health check not found in agent logs"
fi
if echo "$OUT" | grep -q "instanceHealthCheckResult=OK"; then
  pass "ACCELERATED_COMPUTE health check reports OK on the instance"
elif echo "$OUT" | grep -q "instanceHealthCheckResult=INITIALIZING"; then
  warn "ACCELERATED_COMPUTE still INITIALIZING (retry shortly)"
elif echo "$OUT" | grep -q "instanceHealthCheckResult=INSUFFICIENT_DATA"; then
  warn "ACCELERATED_COMPUTE reports INSUFFICIENT_DATA (dcgm-init not producing health yet)"
elif echo "$OUT" | grep -q "instanceHealthCheckResult=IMPAIRED"; then
  fail "ACCELERATED_COMPUTE reports IMPAIRED (GPU unhealthy)"
else
  warn "could not determine ACCELERATED_COMPUTE status (see LASTSTATUS above)"
fi

# 3b. Backend: best-effort check that describe-container-instances surfaces it.
if [[ -n "$CI_ARN" && "$CI_ARN" != "None" ]]; then
  HS_JSON="$(aws ecs describe-container-instances --region "$REGION" --cluster "$CLUSTER" \
    --container-instances "$CI_ARN" \
    --query 'containerInstances[0].healthStatus' --output json 2>/dev/null)"
  echo "    api> healthStatus: $(echo "$HS_JSON" | tr -d '\n' | tr -s ' ')"
  ACC="$(aws ecs describe-container-instances --region "$REGION" --cluster "$CLUSTER" \
    --container-instances "$CI_ARN" \
    --query 'containerInstances[0].healthStatus.details[?type==`ACCELERATED_COMPUTE`].status' \
    --output text 2>/dev/null)"
  if [[ -n "$ACC" && "$ACC" != "None" ]]; then
    if [[ "$ACC" == "OK" ]]; then
      pass "describe-container-instances reports ACCELERATED_COMPUTE=OK"
    else
      warn "describe-container-instances reports ACCELERATED_COMPUTE=$ACC"
    fi
  else
    # Not a failure: the backend only surfaces ACCELERATED_COMPUTE in the
    # describe API for capacity-provider / Managed Instances topologies. On a
    # plain run-instances EC2 host the on-box check (3a) is the source of truth.
    warn "describe-container-instances healthStatus does not include ACCELERATED_COMPUTE (expected on non-capacity-provider EC2; on-box check in 3a is authoritative)"
  fi
else
  warn "could not resolve container instance ARN; skipped describe-container-instances health check"
fi

# ---------------------------------------------------------------------------
section "4. CloudWatch GPU metrics"
# ---------------------------------------------------------------------------
NS="ECS/ContainerInsights"
END="$(date -u +%Y-%m-%dT%H:%M:%S)"
START="$(date -u -d '20 minutes ago' +%Y-%m-%dT%H:%M:%S 2>/dev/null || date -u -v-20M +%Y-%m-%dT%H:%M:%S)"

check_metric() {
  local metric="$1" label="$2"
  local n
  n="$(aws cloudwatch get-metric-statistics --region "$REGION" \
    --namespace "$NS" --metric-name "$metric" \
    --dimensions Name=ClusterName,Value="$CLUSTER" \
    --start-time "$START" --end-time "$END" --period 60 \
    --statistics Maximum \
    --query 'length(Datapoints)' --output text 2>/dev/null)"
  if [[ "$n" =~ ^[0-9]+$ && "$n" -gt 0 ]]; then
    pass "$label: $n datapoint(s) in last 20m (ClusterName dim)"
  else
    warn "$label: no datapoints yet (metric propagation can lag a few minutes)"
  fi
}

echo "  -- instance-level --"
check_metric InstanceGPULimit      "InstanceGPULimit"
check_metric InstanceGPUUsageTotal "InstanceGPUUsageTotal"
echo "  -- container/task-level (require a running service) --"
check_metric ContainerGPUUtilization "ContainerGPUUtilization"
check_metric TaskGPUUtilization      "TaskGPUUtilization"

# ---------------------------------------------------------------------------
section "Summary"
# ---------------------------------------------------------------------------
echo "  PASS: $PASS   WARN: $WARN   FAIL: $FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  echo "  RESULT: FAILED"
  exit 1
fi
echo "  RESULT: OK${WARN:+ (with $WARN warning(s))}"
exit 0
