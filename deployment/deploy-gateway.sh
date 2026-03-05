#!/bin/bash
set -euo pipefail

# Nano Cloud Gateway Deployment Script

# Variables
EC2_USER="${EC2_USER:-ubuntu}"
EC2_HOST="${EC2_HOST:-ec2-43-200-3-149.ap-northeast-2.compute.amazonaws.com}"
PEM_FILE="${PEM_FILE:-~/Downloads/web-crawler.pem}"
DEPLOY_DIR="${DEPLOY_DIR:-/home/ubuntu/nano-gateway}"
LOCAL_PROJECT_DIR="${LOCAL_PROJECT_DIR:-${GITHUB_WORKSPACE:-$(pwd)}}"
if [ -d "$LOCAL_PROJECT_DIR/cmd/gateway" ]; then
    PROJECT_ROOT="$LOCAL_PROJECT_DIR"
elif [ -d "$LOCAL_PROJECT_DIR/nano-cloud/cmd/gateway" ]; then
    PROJECT_ROOT="$LOCAL_PROJECT_DIR/nano-cloud"
else
    PROJECT_ROOT="$(pwd)"
fi
GATEWAY_TOKEN="${GATEWAY_TOKEN:-}"
GATEWAY_LOG_FILE="${GATEWAY_LOG_FILE:-$DEPLOY_DIR/logs/gateway.log}"
GATEWAY_LOG_LEVEL="${GATEWAY_LOG_LEVEL:-info}"
GATEWAY_LOG_MAX_SIZE="${GATEWAY_LOG_MAX_SIZE:-50}"
GATEWAY_LOG_MAX_BACKUPS="${GATEWAY_LOG_MAX_BACKUPS:-10}"
GATEWAY_LOG_MAX_AGE="${GATEWAY_LOG_MAX_AGE:-14}"
GATEWAY_LOG_COMPRESS="${GATEWAY_LOG_COMPRESS:-true}"
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"
BINARY_NAME="${BINARY_NAME:-gateway}"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() {
    echo -e "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Require token unless explicit dev mode
if [ -z "${DEV_MODE:-}" ] && [ -z "$GATEWAY_TOKEN" ]; then
    log "${RED}GATEWAY_TOKEN is required${NC}"
    exit 1
fi

# 1. Build
build() {
    log "${YELLOW}Building Gateway binary...${NC}"
    cd "$PROJECT_ROOT"
    
    mkdir -p bin
    CGO_ENABLED=0 GOOS=$TARGET_OS GOARCH=$TARGET_ARCH go build \
        -ldflags "-s -w" \
        -o bin/$BINARY_NAME ./cmd/gateway

    if [ $? -ne 0 ]; then
        log "${RED}Build failed${NC}"
        return 1
    fi
    log "${GREEN}Build successful${NC}"
}

# 2. Transfer
transfer() {
    log "${YELLOW}Transferring files to $EC2_HOST...${NC}"
    
    # Create dir
    ssh -o StrictHostKeyChecking=no -i "$PEM_FILE" "$EC2_USER@$EC2_HOST" "mkdir -p $DEPLOY_DIR"
    
    # Stop service if running
    ssh -o StrictHostKeyChecking=no -i "$PEM_FILE" "$EC2_USER@$EC2_HOST" "sudo systemctl stop nano-gateway.service || true"

    # SCP binary
    scp -o StrictHostKeyChecking=no -i "$PEM_FILE" "$PROJECT_ROOT/bin/$BINARY_NAME" "$EC2_USER@$EC2_HOST:$DEPLOY_DIR/$BINARY_NAME"
    
    log "${GREEN}Transfer successful${NC}"
}

# 3. Install & Start
install() {
    log "${YELLOW}Installing and Starting Service...${NC}"
    
    ssh -o StrictHostKeyChecking=no -i "$PEM_FILE" "$EC2_USER@$EC2_HOST" <<EOF
        sudo chmod +x $DEPLOY_DIR/$BINARY_NAME

        sudo mkdir -p "$(dirname "$GATEWAY_LOG_FILE")"
        sudo chown -R $EC2_USER:$EC2_USER "$(dirname "$GATEWAY_LOG_FILE")"
        sudo mkdir -p "$DEPLOY_DIR/data"
        
        # Create Systemd Service
        sudo tee /etc/systemd/system/nano-gateway.service >/dev/null <<SERVICE
[Unit]
Description=Nano Cloud Gateway
After=network.target

[Service]
Type=simple
User=$EC2_USER
WorkingDirectory=$DEPLOY_DIR
ExecStart=$DEPLOY_DIR/$BINARY_NAME -addr :8081 -token "$GATEWAY_TOKEN" -config-store-dir "$DEPLOY_DIR/data" -log-file "$GATEWAY_LOG_FILE" -log-level "$GATEWAY_LOG_LEVEL" -log-max-size $GATEWAY_LOG_MAX_SIZE -log-max-backups $GATEWAY_LOG_MAX_BACKUPS -log-max-age $GATEWAY_LOG_MAX_AGE -log-compress $GATEWAY_LOG_COMPRESS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SERVICE

        # Reload and Start
        sudo systemctl daemon-reload
        sudo systemctl enable nano-gateway.service
        sudo systemctl start nano-gateway.service
        sudo systemctl status nano-gateway.service --no-pager
EOF
    log "${GREEN}Deployment completed!${NC}"
}



# Main
case "${1:-deploy}" in
    "deploy")
        build
        transfer
        install
        ;;
    *)
        echo "Usage: $0 [deploy]"
        exit 1
        ;;
esac
