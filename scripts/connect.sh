#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Check for .env file
if [ ! -f .env ]; then
  if [ -f .env.example ]; then
    echo -e "${YELLOW}No .env file found. Creating from .env.example...${NC}"
    cp .env.example .env
  else
    echo -e "${RED}Error: .env.example not found.${NC}"
    exit 1
  fi
fi

# Function to read variable from .env
get_var() {
  local key=$1
  local val=$(grep "^${key}=" .env | cut -d= -f2-)
  echo "$val"
}

# Function to update variable in .env
set_var() {
  local key=$1
  local val=$2
  # Escape slashes for sed
  val_escaped=$(echo "$val" | sed 's/[\/&]/\\&/g')
  if grep -q "^${key}=" .env; then
    sed -i.bak "s|^${key}=.*|${key}=${val_escaped}|" .env && rm .env.bak
  else
    echo "${key}=${val}" >> .env
  fi
}

normalize_relay_url() {
  local raw="$1"
  local normalized
  normalized=$(printf '%s' "$raw" | sed -E 's/^[[:space:]]+|[[:space:]]+$//g')
  while [ -n "$normalized" ] && [[ "${normalized:0:1}" == '"' || "${normalized:0:1}" == "'" || "${normalized:0:1}" == '`' ]]; do
    normalized="${normalized:1}"
  done
  while [ -n "$normalized" ] && [[ "${normalized: -1}" == '"' || "${normalized: -1}" == "'" || "${normalized: -1}" == '`' ]]; do
    normalized="${normalized%?}"
  done
  if [ -z "$normalized" ]; then
    echo ""
    return
  fi
  if [[ "$normalized" != *"://"* ]]; then
    normalized="ws://$normalized"
  fi
  local scheme="${normalized%%://*}"
  local rest="${normalized#*://}"
  local scheme_lc
  scheme_lc=$(printf '%s' "$scheme" | tr '[:upper:]' '[:lower:]')
  case "$scheme_lc" in
    ws|wss|http|https) ;;
    *)
      echo "$normalized"
      return
      ;;
  esac
  local authority
  authority=$(printf '%s' "$rest" | sed -E 's#[/?#].*$##')
  local suffix="${rest#"$authority"}"
  local userinfo=""
  local hostport="$authority"
  if [[ "$authority" == *"@"* ]]; then
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
  fi
  local has_port=0
  if [[ "$hostport" =~ ^\[[^]]+\]:[0-9]+$ ]]; then
    has_port=1
  elif [[ "$hostport" =~ ^\[[^]]+\]$ ]]; then
    has_port=0
  elif [[ "$hostport" == *:* ]]; then
    has_port=1
  fi
  authority="$hostport"
  if [ -n "$userinfo" ]; then
    authority="${userinfo}@${hostport}"
  fi
  normalized="${scheme}://${authority}${suffix}"
  echo "$normalized"
}

relay_scheme() {
  local url="$1"
  printf '%s' "${url%%://*}" | tr '[:upper:]' '[:lower:]'
}

relay_authority() {
  local url="$1"
  local rest="${url#*://}"
  printf '%s' "$rest" | sed -E 's#[/?#].*$##'
}

relay_hostport() {
  local authority
  authority=$(relay_authority "$1")
  if [[ "$authority" == *"@"* ]]; then
    printf '%s' "${authority##*@}"
    return
  fi
  printf '%s' "$authority"
}

relay_has_port() {
  local hostport
  hostport=$(relay_hostport "$1")
  if [[ "$hostport" =~ ^\[[^]]+\]:[0-9]+$ ]]; then
    return 0
  fi
  if [[ "$hostport" =~ ^\[[^]]+\]$ ]]; then
    return 1
  fi
  if [[ "$hostport" == *:* ]]; then
    return 0
  fi
  return 1
}

relay_with_port() {
  local url="$1"
  local port="$2"
  if relay_has_port "$url"; then
    echo "$url"
    return
  fi
  local scheme="${url%%://*}"
  local rest="${url#*://}"
  local authority
  authority=$(printf '%s' "$rest" | sed -E 's#[/?#].*$##')
  local suffix="${rest#"$authority"}"
  local userinfo=""
  local hostport="$authority"
  if [[ "$authority" == *"@"* ]]; then
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
  fi
  hostport="${hostport}:${port}"
  if [ -n "$userinfo" ]; then
    authority="${userinfo}@${hostport}"
  else
    authority="$hostport"
  fi
  echo "${scheme}://${authority}${suffix}"
}

relay_is_local_host() {
  local hostport
  hostport=$(relay_hostport "$1")
  local host="$hostport"
  if [[ "$host" =~ ^\[[^]]+\]$ ]]; then
    host="${host:1:${#host}-2}"
  elif [[ "$host" =~ ^\[[^]]+\]:[0-9]+$ ]]; then
    host="${host%%]*}"
    host="${host#[}"
  elif [[ "$host" == *:* ]]; then
    host="${host%%:*}"
  fi
  case "$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')" in
    localhost|127.0.0.1|::1) return 0 ;;
    *) return 1 ;;
  esac
}

relay_to_http_base() {
  local url="$1"
  local scheme
  scheme=$(relay_scheme "$url")
  local authority
  authority=$(relay_authority "$url")
  case "$scheme" in
    ws) echo "http://${authority}" ;;
    wss) echo "https://${authority}" ;;
    http|https) echo "${scheme}://${authority}" ;;
    *) echo "" ;;
  esac
}

probe_gateway_relay() {
  local relay="$1"
  if ! command -v curl >/dev/null 2>&1; then
    return 0
  fi
  local base
  base=$(relay_to_http_base "$relay")
  if [ -z "$base" ]; then
    return 1
  fi
  local code
  code=$(curl -sS -o /dev/null -w "%{http_code}" --connect-timeout 3 --max-time 5 "${base}/v1/workers" || true)
  case "$code" in
    200|401|403) return 0 ;;
    *) return 1 ;;
  esac
}

resolve_relay_url() {
  local input="$1"
  local candidates=""
  append_candidate() {
    local c="$1"
    if [ -z "$c" ]; then
      return
    fi
    if printf '%s\n' "$candidates" | grep -Fxq "$c"; then
      return
    fi
    if [ -z "$candidates" ]; then
      candidates="$c"
    else
      candidates="${candidates}
$c"
    fi
  }

  local scheme
  scheme=$(relay_scheme "$input")
  if relay_has_port "$input"; then
    append_candidate "$input"
  else
    if relay_is_local_host "$input"; then
      append_candidate "$(relay_with_port "$input" 8081)"
      append_candidate "$input"
    else
      case "$scheme" in
        ws)
          append_candidate "$(relay_with_port "$input" 8081)"
          append_candidate "$input"
          append_candidate "wss://$(relay_authority "$input")"
          ;;
        wss)
          append_candidate "$input"
          append_candidate "$(relay_with_port "ws://$(relay_authority "$input")" 8081)"
          ;;
        http)
          append_candidate "$(relay_with_port "$input" 8081)"
          append_candidate "$input"
          append_candidate "https://$(relay_authority "$input")"
          ;;
        https)
          append_candidate "$input"
          append_candidate "$(relay_with_port "http://$(relay_authority "$input")" 8081)"
          ;;
        *)
          append_candidate "$input"
          ;;
      esac
    fi
  fi

  local best=""
  while IFS= read -r c; do
    if [ -z "$c" ]; then
      continue
    fi
    if [ -z "$best" ]; then
      best="$c"
    fi
    if probe_gateway_relay "$c"; then
      best="$c"
      break
    fi
  done <<EOF
$candidates
EOF
  if [ -z "$best" ]; then
    best="$input"
  fi
  echo "$best"
}

echo -e "${BLUE}=== Nano Cloud Connection Wizard ===${NC}"
echo "This script will help you configure your worker to connect to a Nano Cloud Gateway."
echo ""

# 1. Gateway URL
CURRENT_RELAY=$(get_var "RELAY_URL")
if [ -z "$CURRENT_RELAY" ]; then CURRENT_RELAY="ws://localhost:8081"; fi
read -p "Gateway URL [${CURRENT_RELAY}]: " NEW_RELAY
NEW_RELAY=${NEW_RELAY:-$CURRENT_RELAY}
NEW_RELAY=$(normalize_relay_url "$NEW_RELAY")
NEW_RELAY=$(resolve_relay_url "$NEW_RELAY")
set_var "RELAY_URL" "$NEW_RELAY"
export RELAY_URL="$NEW_RELAY"

# 2. LLM Configuration (OpenAI Compatible)
echo ""
echo -e "${YELLOW}LLM Configuration${NC}"
echo "Configure the LLM endpoint for the agent runtime."

# Base URL
CURRENT_BASE=$(get_var "NANO_BASE_URL")
if [ -z "$CURRENT_BASE" ]; then CURRENT_BASE="https://api.openai.com/v1"; fi
read -p "LLM Base URL [${CURRENT_BASE}]: " NEW_BASE
NEW_BASE=${NEW_BASE:-$CURRENT_BASE}
set_var "NANO_BASE_URL" "$NEW_BASE"
export NANO_BASE_URL="$NEW_BASE"

# API Key
CURRENT_KEY=$(get_var "NANO_API_KEY")
# If variable is empty in .env, check shell env
if [ -z "$CURRENT_KEY" ]; then CURRENT_KEY="${NANO_API_KEY}"; fi

# Mask key for display
MASKED_KEY=""
if [ -n "$CURRENT_KEY" ]; then
  MASKED_KEY="${CURRENT_KEY:0:3}...${CURRENT_KEY: -3}"
else
  MASKED_KEY="(empty)"
fi

read -p "LLM API Key [${MASKED_KEY}]: " NEW_KEY
if [ -n "$NEW_KEY" ]; then
  set_var "NANO_API_KEY" "$NEW_KEY"
  export NANO_API_KEY="$NEW_KEY"
else
  export NANO_API_KEY="$CURRENT_KEY"
fi

# Model
CURRENT_MODEL=$(get_var "NANO_MODEL")
if [ -z "$CURRENT_MODEL" ]; then CURRENT_MODEL="gpt-4o"; fi
read -p "Model Name [${CURRENT_MODEL}]: " NEW_MODEL
NEW_MODEL=${NEW_MODEL:-$CURRENT_MODEL}
set_var "NANO_MODEL" "$NEW_MODEL"
export NANO_MODEL="$NEW_MODEL"

echo ""
CURRENT_MIRROR=$(get_var "DOCKERHUB_MIRROR")
if [ -z "$CURRENT_MIRROR" ]; then CURRENT_MIRROR="docker.m.daocloud.io"; fi
read -p "Docker Hub Mirror [${CURRENT_MIRROR}]: " NEW_MIRROR
NEW_MIRROR=${NEW_MIRROR:-$CURRENT_MIRROR}
NEW_MIRROR=${NEW_MIRROR%/}
set_var "DOCKERHUB_MIRROR" "$NEW_MIRROR"
export DOCKERHUB_MIRROR="$NEW_MIRROR"
set_var "BASE_GOLANG_IMAGE" "${NEW_MIRROR}/library/golang:1.24.4"
export BASE_GOLANG_IMAGE="${NEW_MIRROR}/library/golang:1.24.4"
set_var "BASE_LINUX_IMAGE" "${NEW_MIRROR}/library/golang:1.24.4"
export BASE_LINUX_IMAGE="${NEW_MIRROR}/library/golang:1.24.4"
CURRENT_DOCKER_CLI_VERSION=$(get_var "DOCKER_CLI_VERSION")
if [ -z "$CURRENT_DOCKER_CLI_VERSION" ]; then CURRENT_DOCKER_CLI_VERSION="28.5.1"; fi
set_var "DOCKER_CLI_VERSION" "$CURRENT_DOCKER_CLI_VERSION"
export DOCKER_CLI_VERSION="$CURRENT_DOCKER_CLI_VERSION"
set_var "DOCKER_CLI_IMAGE" "${NEW_MIRROR}/library/docker:${CURRENT_DOCKER_CLI_VERSION}-cli"
export DOCKER_CLI_IMAGE="${NEW_MIRROR}/library/docker:${CURRENT_DOCKER_CLI_VERSION}-cli"

CURRENT_GOPROXY=$(get_var "GOPROXY")
if [ -z "$CURRENT_GOPROXY" ]; then CURRENT_GOPROXY="https://goproxy.cn,direct"; fi
read -p "Go Module Proxy [${CURRENT_GOPROXY}]: " NEW_GOPROXY
NEW_GOPROXY=${NEW_GOPROXY:-$CURRENT_GOPROXY}
set_var "GOPROXY" "$NEW_GOPROXY"
export GOPROXY="$NEW_GOPROXY"

# 3. Nano Agent Advanced Config
echo ""
echo -e "${YELLOW}Nano Agent Advanced Configuration${NC}"
if [ ! -f nano-agent.env ]; then
  touch nano-agent.env
  echo "# Nano Agent Advanced Configuration" > nano-agent.env
  echo "# Add NANO_xxx environment variables here" >> nano-agent.env
fi

read -p "Do you want to configure advanced Nano Agent settings? [y/N]: " CONFIGURE_ADVANCED
if [[ "$CONFIGURE_ADVANCED" =~ ^[Yy]$ ]]; then
  echo "Opening nano-agent.env in editor..."
  if [ -n "$EDITOR" ]; then
    $EDITOR nano-agent.env
  elif command -v nano >/dev/null; then
    nano nano-agent.env
  elif command -v vim >/dev/null; then
    vim nano-agent.env
  else
    echo "No editor found. Please edit nano-agent.env manually."
  fi
fi

# 4. Optional: Proxy

echo ""
echo -e "${GREEN}Configuration saved to .env${NC}"

COMPOSE_CMD=""
if command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD="docker-compose"
elif command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD="docker compose"
else
  echo -e "${RED}Error: neither docker-compose nor docker compose is available in PATH.${NC}"
  exit 1
fi

# Detect if local gateway
IS_LOCAL=0
if [[ "$NEW_RELAY" == *"localhost"* ]] || [[ "$NEW_RELAY" == *"127.0.0.1"* ]]; then
  IS_LOCAL=1
fi

if [ "$IS_LOCAL" -eq 1 ]; then
  echo -e "${BLUE}Detected local gateway URL.${NC}"
  echo "Starting Full Stack (Gateway + Worker)..."
  echo ""
  # Inside Docker the worker container cannot reach "localhost" or "127.0.0.1" –
  # those resolve to the container's own loopback, not the gateway.  Extract the
  # port the user configured (normalize_relay_url always adds one) and rewrite
  # the host to the Compose service name so the port is correctly passed through.
  local_port=$(echo "$NEW_RELAY" | grep -oE ':[0-9]+' | head -1 | tr -d ':')
  if [ -z "$local_port" ]; then local_port="8081"; fi
  RELAY_URL="ws://gateway:${local_port}" ${COMPOSE_CMD} up --build -d
  echo ""
  echo -e "${GREEN}Full stack is running in the background.${NC}"
  echo -e "Use '${COMPOSE_CMD} logs -f' to view logs."
else
  echo -e "${BLUE}Detected remote gateway URL.${NC}"
  echo "Starting Worker Only..."
  echo ""
  ${COMPOSE_CMD} up worker --build --no-deps -d
  echo ""
  echo -e "${GREEN}Worker is running in the background.${NC}"
  echo -e "Use '${COMPOSE_CMD} logs -f worker' to view logs."
fi
