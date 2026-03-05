#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}Nano Cloud Worker Setup${NC}"
echo "This script will help you generate a worker configuration file."
echo ""

# Defaults
DEFAULT_RELAY="ws://localhost:8081"
DEFAULT_WORKSPACE="/tmp/nano-workspaces"
DEFAULT_STATE_DIR="${HOME}/.nano-cloud/state"

# Interactive Prompts
read -p "Relay URL [${DEFAULT_RELAY}]: " RELAY_URL
RELAY_URL=${RELAY_URL:-$DEFAULT_RELAY}

read -p "Enroll Token: " ENROLL_TOKEN

read -p "Worker ID (optional, leave blank for auto): " WORKER_ID

read -p "Labels (comma-separated, optional): " LABELS

read -p "Workspace Root [${DEFAULT_WORKSPACE}]: " WORKSPACE_ROOT
WORKSPACE_ROOT=${WORKSPACE_ROOT:-$DEFAULT_WORKSPACE}

read -p "State Dir [${DEFAULT_STATE_DIR}]: " STATE_DIR
STATE_DIR=${STATE_DIR:-$DEFAULT_STATE_DIR}

read -p "Nano API Key (optional, for env passthrough): " NANO_API_KEY

echo ""
echo -e "${BLUE}Generating worker-config.yaml...${NC}"

cat > worker-config.yaml <<EOF
relay_url: "${RELAY_URL}"
enroll_token: "${ENROLL_TOKEN}"
worker_id: "${WORKER_ID}"
workspace_root: "${WORKSPACE_ROOT}"
state_dir: "${STATE_DIR}"
labels:
EOF

if [ -n "$LABELS" ]; then
    IFS=',' read -ra LABEL_ARR <<< "$LABELS"
    for label in "${LABEL_ARR[@]}"; do
        label="$(echo "$label" | xargs)"
        if [ -n "$label" ]; then
            echo "  - $label" >> worker-config.yaml
        fi
    done
else
    echo "  - docker-desktop" >> worker-config.yaml
fi

echo -e "${GREEN}Success! worker-config.yaml created.${NC}"
echo ""
echo "To start the worker:"
echo "1. Ensure you have the runtime images built (see README)"
echo "2. Export your API keys (e.g., export NANO_API_KEY=...)"
echo "3. Run: ./worker -config worker-config.yaml"
