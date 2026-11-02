# Nano Cloud Local Debugging

[中文](./LOCAL_DEBUG.zh-CN.md)

## Quick end-to-end run (Gateway → Worker → Run → SSE)

```bash
cd /Users/user/CodeProjects/library/nano-cloud
./scripts/local-e2e.sh
```

The output includes:
- enroll token
- run_id
- SSE events URL
- console URL

The script uses the `nano_agent` runtime (`nano-agent-runtime:local`) by default, and reads nano-agent environment variables from `../nano-agent/.env` (overridable via `NANO_AGENT_ENV_FILE`).
## Manual steps (for step-by-step breakpoints/logging)

### 1) Start the Gateway

```bash
cd /Users/user/CodeProjects/library/nano-cloud
go build -o bin/gateway ./cmd/gateway
./bin/gateway -addr :8081 -token dev-token -config-store-dir ./data
```

Console:
`curl http://localhost:8081/console`

Optional: enable console login to view sensitive information (anonymous access only shows the public overview):

```bash
export CONSOLE_USERNAME=admin
export CONSOLE_PASSWORD=change-me
export CONSOLE_SESSION_TTL_MINUTES=480
```

Open `http://localhost:8081/console` in a browser; after logging in on the page, sensitive fields become visible.
### 2) Create an enroll token (admin)

```bash
curl -sS -X POST "http://localhost:8081/v1/admin/enroll-tokens" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"ttl_seconds":3600}'
```

### 3) Start the Worker (remote bootstrap)

```bash
cd /Users/user/CodeProjects/library/nano-cloud
go build -o bin/worker ./cmd/worker
./bin/worker -relay "ws://localhost:8081" -enroll-token "<token>" -state-dir "$HOME/.nano-cloud/state"
```

### 4) Create a Run and subscribe to SSE events

```bash
curl -sS -X POST "http://localhost:8081/v1/runs" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"runtime":"nano_agent","prompt":"hello"}'
```

```bash
curl -N -H "Authorization: Bearer dev-token" "http://localhost:8081/v1/runs/<run_id>/events"
```

### 5) Cancel a Run

```bash
curl -sS -X POST "http://localhost:8081/v1/runs/<run_id>/cancel" \
  -H "Authorization: Bearer dev-token"
```
