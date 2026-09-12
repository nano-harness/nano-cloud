# 安全加固

[English](./SECURITY-HARDENING.md)

本文档说明如何加固运行**不可信的、模型生成代码**的 Nano Cloud 部署。文中只描述本仓库真实存在的机制，每节都标注了对应代码位置。

## 威胁模型简述

Worker 在 Docker 容器中执行 agent runtime。Agent 代码——shell 命令、脚本、运行时安装的依赖——本质上是模型输出，必须视为不可信。标准 Docker 容器（runc）与宿主机共享内核，一旦遇到内核漏洞或容器逃逸 CVE，整个 Worker 宿主机都可能被攻陷。对多租户或高风险环境，2026 年的业界基线是：沙箱化 OCI 运行时（gVisor `runsc`）或 MicroVM（Firecracker/Kata）+ 短时效 scoped 凭据 + 出站网络分段。

## 容器运行时隔离：runsc / gVisor

Worker 支持通过 worker 配置中的 `container_runtime` 键为整台 worker 选择备用 OCI 运行时（`pkg/worker/config.go`，`Config.ContainerRuntime`）：

```yaml
# worker-config.yaml
container_runtime: runsc   # gVisor；留空 = docker 默认（runc）
```

设置后，每个 agent 容器都会带 `--runtime=<值>` 启动（`pkg/worker/docker_runner.go` 的 `DockerRuntimeArgs`，在 `pkg/worker/worker.go` 的 `handleRunRequest` 中接线）。注意：

- 值原样透传。如果该运行时未安装或未注册到 docker daemon，`docker run` 会报 `unknown or invalid runtime name: ...`；Worker 会以 `DOCKER_RUN_FAILED` 透传该错误并附带一条提示（`ContainerRuntimeErrorHint`）。可在 Worker 宿主机上用 `docker info` 的 Runtimes 一栏确认。
- 只作用于 **agent 容器**。网络策略 sidecar 代理（见下）是受信基础设施，保持默认运行时。
- `runsc` 需要 Linux Docker 宿主机（或手动安装了 gVisor 的 Docker Desktop Linux VM）。其他已注册的运行时（如 Kata Containers）用法相同——任何 `docker run --runtime` 接受的名字都有效。
- MicroVM（Firecracker、Kata）提供比 gVisor 更强的隔离（独立 guest 内核）。本仓库未内置；只要在宿主机上将其注册为 docker 运行时，`container_runtime` 即可直接选用，无需改代码。
- `exec` runner（`runtimes.<name>.runner: exec`）直接在宿主机上运行 agent，**完全没有容器隔离**。切勿用于不可信负载。

运行时选择是按 worker 的部署决策，而不是按 run：proto 的 `Policy`（`proto/runtime/v1/runtime.proto`）刻意不包含容器运行时字段，因此提交 run 的客户端无法削弱已加固 worker 的隔离级别。

## 网络策略矩阵

`RunRequest.policy.network`（`enum NetworkPolicy`）到 worker 行为的映射（`DockerExtraArgsFromPolicy` + `handleRunRequest` 的 allowlist 分支）：

| 策略 | Docker 接线 | 实际出站能力 |
| --- | --- | --- |
| `NETWORK_POLICY_NONE` | `--network none` | 完全无网络（仅 loopback）。 |
| `NETWORK_POLICY_ALLOWLIST` | internal 网络 `<容器名>-allow` + `net-policy-proxy` sidecar；agent 获得 `HTTP(S)_PROXY=http://proxy:3128` | 仅 worker `network_allowlist` 中的主机/CIDR，默认 TCP 443。 |
| `NETWORK_POLICY_ALL` | 无额外参数 | Docker 默认（bridge/NAT）完整出站。 |
| `NETWORK_POLICY_UNSPECIFIED` | 无额外参数 | 与 `ALL` 相同——**默认是放开的**。 |

`Policy.resources` 的资源限制与此正交，存在时总会应用：`--cpus`、`--memory`、`--pids-limit`。

### ALLOWLIST 与 net-policy-proxy 的关系

`ALLOWLIST` 由 `net-policy-proxy` 组件强制执行（`cmd/net-policy-proxy`，镜像为 `network_policy_image`，默认 `nano-net-policy-runtime:local`，由 `docker/net-policy-runtime` 构建）：

1. Worker 创建一个 **docker internal 网络**（无出站路由）。
2. 在该网络上启动代理，并把代理额外接入默认 `bridge` 网络，因此只有代理能访问互联网。
3. Agent 容器只加入 internal 网络；其 `HTTP_PROXY`/`HTTPS_PROXY`（及小写变体）指向该代理。
4. 代理读取 `network_allowlist`（挂载为 `/etc/nano/allowlist.json`），只允许指向白名单主机（精确匹配或 `*.` 通配）或 CIDR 的 CONNECT/HTTP 请求，仅 TCP，端口默认 443。规则由 `ValidateNetworkAllowlist`（`pkg/worker/net_allowlist.go`）校验；白名单为空或非法时 run 直接以 `NETWORK_ALLOWLIST_EMPTY` / `NETWORK_ALLOWLIST_INVALID` 失败，而不是静默放开出站。

依赖它之前需要了解的注意事项：

- 该机制是网络层阻断（internal 网络）+ 应用层过滤（代理环境变量）的组合。忽略 `HTTP(S)_PROXY` 的进程无法直接出网——internal 网络没有路由——但仍能到达代理，因此白名单务必收紧。
- 基于代理的白名单存在经由代理 DNS 解析进行 UDP/DNS 外带（exfiltration）的已知弱点；如需防 DNS 隧道外泄，优先用 `NETWORK_POLICY_NONE` 或在宿主机层加出站防火墙。
- 代理是按 run 启动的 sidecar，随 run 结束一起回收（`handleRunRequest`/`handleCancel` 的完成与取消路径）。

## 凭据：不进容器、短时效

当前凭据的实际流向（均为真实机制）：

- **`env_passthrough`**（worker 配置）：把指定的宿主机环境变量复制进每个 agent 容器。这是 LLM API key 进入容器的主要途径——列在这里的变量对容器内不可信代码完全可见。务必最小化该列表；切勿放入云厂商密钥（AWS/GCP）、SSH agent 变量或 docker 凭据。
- **`runtimes.<name>.env` / `env_file`**（worker 配置）：runtime 的静态环境变量。`env_file`（`pkg/worker/runtime/nano_agent.go` 的 `NanoAgentAdapter`）把值放在 worker 宿主机磁盘上而不是 YAML 里，但最终仍以容器环境变量形式出现。
- **`worker quickstart` 生成的 `agent-config.yaml`** 用 `${ENV_VAR}` 占位符引用密钥而非内嵌；实际值仍通过 `env_passthrough` 传入。
- proto 的 `RunRequest` 中有 **`secrets_ref`** 字段，但仅是 wire 层占位，worker **目前未实现**——不要假设存在密钥注入机制。

针对不可信负载的建议：

1. 发放**短时效、scope 受限的 LLM token**（如由代理签发的带速率/额度限制的 key），而不是把长期主 API key 放进 `env_passthrough`。这样即使容器被打穿，泄露的也只是很快过期且可吊销的 token。
2. 优先使用 `NETWORK_POLICY_ALLOWLIST` 且只列你的 LLM 端点，这样泄露的 key 至少无法被外传到任意主机。
3. Worker↔Gateway 认证：worker 用 `token` 连接（WebSocket 上的 `Authorization: Bearer`，`pkg/worker/worker.go`）。新 worker 通过 Console 审批的**配对码**入网，配对码 **15 分钟过期**（`pkg/server/pairing_store.go` 的 `PairingTTL`）——请使用配对流程，而不是分发共享的长期 token；worker 宿主机下线时轮换 `token`。

## 与 nano-agent 沙箱的分层关系

隔离是两层独立的纵深防御：

1. **外层——nano-cloud worker（本仓库）。** 容器隔离（runc，或经 `container_runtime` 使用 `runsc`/MicroVM）、网络策略（上表）、资源限制。这一层由 docker daemon / 内核 / gVisor 强制，与 agent 进程内部行为无关。
2. **内层——nano-agent（兄弟仓库，运行在容器内）。** nano-agent 对单次工具调用做沙箱：Linux 上用 bubblewrap（`bwrap`）profile，macOS 上用 `sandbox-exec`（见 nano-agent 仓库的 `pkg/sandbox`）；外加审批策略（`Policy.approval`：`auto`/`ask`/`deny`），由 worker 以 `APPROVAL_POLICY` 环境变量传入容器（`pkg/worker/runtime/base.go`）。

两层互补：内层沙箱限制 agent *工具调用*能触达的范围，即使 agent 框架被诱导（提示注入）也能兜底；外层容器边界则容纳 agent 进程本身的完全失陷。注意在 Linux runtime 镜像内只有 nano-agent 的 bwrap 路径生效；`sandbox-exec` 适用于 nano-agent 直接跑在 macOS 宿主机的场景。对不可信代码建议两层同开：外层 `container_runtime: runsc`，内层 bwrap + `ask`/`deny` 审批。

## 加固清单

- [ ] Linux worker 宿主机已注册 `runsc`（`docker info` → Runtimes）；
      `worker-config.yaml` 中设置 `container_runtime: runsc`。
- [ ] 不可信 run 使用 `NETWORK_POLICY_NONE`，或 `NETWORK_POLICY_ALLOWLIST`
      + 收紧的 `network_allowlist`。
- [ ] `env_passthrough` 已精简到最少；使用短时效 scoped LLM token；
      列表中不含云厂商/SSH 凭据。
- [ ] Worker 经配对码入网；`token` 不跨宿主机共享。
- [ ] 不可信 runtime 不使用 `runner: exec`。
- [ ] gateway/客户端为每个 run 设置 `Policy.resources`，限制 CPU/内存/pids。
