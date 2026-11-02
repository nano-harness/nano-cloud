#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB_ROOT="$(cd "$ROOT_DIR/.." && pwd)"

GATEWAY_ADDR="${GATEWAY_ADDR:-:8081}"
GATEWAY_HTTP_BASE="${GATEWAY_HTTP_BASE:-http://localhost:8081}"
GATEWAY_TOKEN="${GATEWAY_TOKEN:-dev-token}"
CONFIG_STORE_DIR="${CONFIG_STORE_DIR:-$ROOT_DIR/.workdir/data}"
STATE_DIR="${STATE_DIR:-$ROOT_DIR/.workdir/state}"
RUNTIME="${RUNTIME:-}"
PROMPT="${PROMPT:-hello from nano-agent runtime local e2e}"
NANO_AGENT_ENV_FILE="${NANO_AGENT_ENV_FILE:-$LIB_ROOT/nano-agent/.env}"

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing dependency: $1" >&2; exit 1; }
}

need go
need curl
need python3
need docker

load_nano_agent_env_subset() {
  local env_file="$1"
  if [ ! -f "$env_file" ]; then
    return 1
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "" | \#*) continue ;;
    esac
    local key="${line%%=*}"
    local val="${line#*=}"
    case "$key" in
      NANO_API_KEY|NANO_BASE_URL|NANO_MODEL|NANO_VERBOSE|NANO_REASONING_*|NANO_CONTEXT_*|NANO_HTTP_TIMEOUT|NANO_RESPONSE_TIMEOUT|SERPER_API_KEY|TAVILY_API_KEY)
        export "$key=$val"
        ;;
    esac
  done <"$env_file"
  return 0
}

cd "$ROOT_DIR"

mkdir -p "$ROOT_DIR/bin"

echo "[1/7] build gateway + worker"
go build -o "$ROOT_DIR/bin/gateway" ./cmd/gateway
go build -o "$ROOT_DIR/bin/worker" ./cmd/worker

echo "[2/7] build runtime images"
export DOCKER_BUILDKIT=1
if ! docker image inspect nano-agent-runtime:local >/dev/null 2>&1; then
  docker build --ssh default -t nano-agent-runtime:local -f "$ROOT_DIR/docker/nano-agent-runtime/Dockerfile" "$LIB_ROOT"
fi
if ! docker image inspect nano-cli-runtime:local >/dev/null 2>&1; then
  docker build --ssh default -t nano-cli-runtime:local -f "$ROOT_DIR/docker/cli-runtime/Dockerfile" "$LIB_ROOT"
fi
if ! docker image inspect nano-net-policy-runtime:local >/dev/null 2>&1; then
  docker build --ssh default -t nano-net-policy-runtime:local -f "$ROOT_DIR/docker/net-policy-runtime/Dockerfile" "$LIB_ROOT"
fi

echo "[3/7] start gateway"
mkdir -p "$CONFIG_STORE_DIR"
"$ROOT_DIR/bin/gateway" -addr "$GATEWAY_ADDR" -token "$GATEWAY_TOKEN" -config-store-dir "$CONFIG_STORE_DIR" &
GATEWAY_PID=$!
trap 'kill "$GATEWAY_PID" >/dev/null 2>&1 || true' EXIT

for i in $(seq 1 50); do
  if curl -fsS -o /dev/null "$GATEWAY_HTTP_BASE/console?token=$GATEWAY_TOKEN"; then
    break
  fi
  sleep 0.1
done

echo "[4/7] create pairing request (manual simulation)"
PAIRING_JSON="$(
  curl -sS -X POST "$GATEWAY_HTTP_BASE/v1/worker/pairing" \
    -H "Content-Type: application/json" \
    -d '{"worker_name":"e2e-worker","host_info":"linux/amd64","labels":["docker-desktop"]}'
)"
PAIRING_ID="$(python3 - <<'PY' "$PAIRING_JSON"
import json,sys
data=json.loads(sys.argv[1])
print(data["id"])
PY
)"
PAIRING_KEY="$(python3 - <<'PY' "$PAIRING_JSON"
import json,sys
data=json.loads(sys.argv[1])
print(data["secret"])
PY
)"
PAIRING_CODE="$(python3 - <<'PY' "$PAIRING_JSON"
import json,sys
data=json.loads(sys.argv[1])
print(data.get("user_code",""))
PY
)"
echo "pairing_id=$PAIRING_ID"
if [ -n "$PAIRING_CODE" ]; then
  echo "user_code=$PAIRING_CODE"
fi

echo "[4.5/7] approve pairing request"
if [ -n "${E2E_APPROVE_METHOD:-}" ] && [ "${E2E_APPROVE_METHOD}" = "code" ] && [ -n "$PAIRING_CODE" ]; then
  curl -sS -X POST "$GATEWAY_HTTP_BASE/v1/admin/pairing/code/$PAIRING_CODE/approve" \
    -H "Authorization: Bearer $GATEWAY_TOKEN" \
    -H "Content-Type: application/json"
else
  curl -sS -X POST "$GATEWAY_HTTP_BASE/v1/admin/pairing/$PAIRING_ID/approve" \
    -H "Authorization: Bearer $GATEWAY_TOKEN" \
    -H "Content-Type: application/json"
fi

echo "[5/7] start worker (remote bootstrap via pairing)"
mkdir -p "$STATE_DIR"
if [ -f "$NANO_AGENT_ENV_FILE" ]; then
  load_nano_agent_env_subset "$NANO_AGENT_ENV_FILE"
else
  echo "WARN: nano-agent env file not found: $NANO_AGENT_ENV_FILE" >&2
  echo "      Set NANO_AGENT_ENV_FILE or export NANO_API_KEY manually." >&2
fi
if [ -z "$RUNTIME" ]; then
  if [ -n "${NANO_API_KEY:-}" ]; then
    RUNTIME="nano_agent"
  else
    RUNTIME="custom"
  fi
fi

# Pre-populate state.json to skip manual pairing in worker (since we already approved it)
# We need to fetch the token first using the secret
WORKER_TOKEN="$(
  curl -sS "$GATEWAY_HTTP_BASE/v1/worker/pairing/$PAIRING_ID" \
    -H "Authorization: Bearer $PAIRING_KEY" | python3 -c 'import sys, json; print(json.load(sys.stdin)["worker_token"])'
)"
echo "worker_token=$WORKER_TOKEN"

cat > "$STATE_DIR/state.json" <<EOF
{
  "worker_id": "",
  "worker_token": "$WORKER_TOKEN",
  "config_version": ""
}
EOF

"$ROOT_DIR/bin/worker" -relay "ws://localhost:8081" -state-dir "$STATE_DIR" -workspace-root "$ROOT_DIR/.workdir/workspace" -log-root "$ROOT_DIR/.workdir/logs" &
WORKER_PID=$!
trap 'kill "$WORKER_PID" >/dev/null 2>&1 || true; kill "$GATEWAY_PID" >/dev/null 2>&1 || true' EXIT

sleep 1

echo "[6/7] create run"
RUN_ID="$(
  curl -sS -X POST "$GATEWAY_HTTP_BASE/v1/runs" \
    -H "Authorization: Bearer $GATEWAY_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"runtime\":\"$RUNTIME\",\"prompt\":\"$PROMPT\"}" | python3 -c 'import sys, json; print(json.load(sys.stdin)["run_id"])'
)"
echo "run_id=$RUN_ID"
echo "events: curl -N -H \"Authorization: Bearer $GATEWAY_TOKEN\" \"$GATEWAY_HTTP_BASE/v1/runs/$RUN_ID/events\""

echo "[7/7] open console"
echo "$GATEWAY_HTTP_BASE/console?token=$GATEWAY_TOKEN"

echo "tail events:"
set +o pipefail
curl -N -H "Authorization: Bearer $GATEWAY_TOKEN" "$GATEWAY_HTTP_BASE/v1/runs/$RUN_ID/events" | python3 /dev/fd/3 3<<'PY'
import json
import sys

event = None
data_lines = []

def handle_event(name, data):
  if name != "run_event":
    return False
  try:
    env = json.loads(data)
  except Exception:
    return False
  kind = (env.get("run_event") or {}).get("kind")
  if kind == "EVENT_KIND_COMPLETED":
    return True
  return False

for raw in sys.stdin:
  line = raw.rstrip("\n")
  if line == "":
    if event is not None and data_lines:
      if handle_event(event, "\n".join(data_lines)):
        sys.exit(0)
    event = None
    data_lines = []
    continue
  if line.startswith(":"):
    continue
  if line.startswith("event:"):
    event = line.split(":", 1)[1].strip()
    continue
  if line.startswith("data:"):
    data_lines.append(line.split(":", 1)[1].lstrip())
    continue
sys.exit(1)
PY
status=$?
set -o pipefail
exit "$status"
