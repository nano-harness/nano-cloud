# Nano Cloud 本地调试

## 快速跑通（Gateway → Worker → Run → SSE）

```bash
cd /Users/user/CodeProjects/library/nano-cloud
./scripts/local-e2e.sh
```

输出里会给出：
- enroll token
- run_id
- SSE events URL
- console URL

脚本默认使用 `nano_agent` runtime（`nano-agent-runtime:local`），并会从 `../nano-agent/.env` 读取 nano-agent 的环境变量（可用 `NANO_AGENT_ENV_FILE` 覆盖）。
## 手工步骤（便于逐段断点/日志）

### 1) 启动 Gateway

```bash
cd /Users/user/CodeProjects/library/nano-cloud
go build -o bin/gateway ./cmd/gateway
./bin/gateway -addr :8081 -token dev-token -config-store-dir ./data
```

Console：
`curl http://localhost:8081/console`

可选：启用 console 登录后查看敏感信息（匿名仅公开概览）：

```bash
export CONSOLE_USERNAME=admin
export CONSOLE_PASSWORD=change-me
export CONSOLE_SESSION_TTL_MINUTES=480
```

浏览器访问 `http://localhost:8081/console`，在页面登录后可查看敏感字段。
### 2) 创建 enroll token（admin）

```bash
curl -sS -X POST "http://localhost:8081/v1/admin/enroll-tokens" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"ttl_seconds":3600}'
```

### 3) 启动 Worker（remote bootstrap）

```bash
cd /Users/user/CodeProjects/library/nano-cloud
go build -o bin/worker ./cmd/worker
./bin/worker -relay "ws://localhost:8081" -enroll-token "<token>" -state-dir "$HOME/.nano-cloud/state"
```

### 4) 创建 Run 并订阅 SSE events

```bash
curl -sS -X POST "http://localhost:8081/v1/runs" \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"runtime":"nano_agent","prompt":"hello"}'
```

```bash
curl -N -H "Authorization: Bearer dev-token" "http://localhost:8081/v1/runs/<run_id>/events"
```

### 5) 取消 Run

```bash
curl -sS -X POST "http://localhost:8081/v1/runs/<run_id>/cancel" \
  -H "Authorization: Bearer dev-token"
```
