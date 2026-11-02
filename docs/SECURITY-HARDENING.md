# Security Hardening

[中文](./SECURITY-HARDENING.zh-CN.md)

This document describes how to harden a Nano Cloud deployment that runs
**untrusted, model-generated code**. It only covers mechanisms that actually
exist in this repository; each section links the relevant code.

## Threat model in one paragraph

The Worker executes agent runtimes in Docker containers. Agent code — shell
commands, scripts, dependencies installed at runtime — is model output and
must be treated as untrusted. A stock Docker container shares the host kernel
(runc), so a kernel exploit or a container-escape CVE can compromise the whole
Worker host. For multi-tenant or high-risk deployments, the 2026 industry
baseline is: a sandboxed OCI runtime (gVisor `runsc`) or a MicroVM
(Firecracker/Kata), short-lived scoped credentials, and egress network
segmentation.

## Container runtime isolation: runsc / gVisor

The Worker supports selecting an alternate OCI runtime per worker via the
`container_runtime` key in the worker config (`pkg/worker/config.go`,
`Config.ContainerRuntime`):

```yaml
# worker-config.yaml
container_runtime: runsc   # gVisor; empty = docker default (runc)
```

When set, every agent container is started with `--runtime=<value>`
(`DockerRuntimeArgs` in `pkg/worker/docker_runner.go`, wired in
`handleRunRequest` in `pkg/worker/worker.go`). Notes:

- The value is passed through verbatim. If the runtime is not installed or
  not registered with the docker daemon, `docker run` fails with
  `unknown or invalid runtime name: ...`; the Worker surfaces this error as
  `DOCKER_RUN_FAILED` plus a hint (`ContainerRuntimeErrorHint`). Check the
  `Runtimes` section of `docker info` on the Worker host.
- It applies to the **agent container only**. The network-policy sidecar
  proxy (see below) is trusted infrastructure and keeps the default runtime.
- `runsc` requires a Linux Docker host (or a Linux Docker Desktop VM with
  gVisor manually installed). Other registered runtimes (e.g. Kata
  Containers) work the same way — any name accepted by `docker run
  --runtime` is valid.
- MicroVMs (Firecracker, Kata) offer even stronger isolation than gVisor
  (dedicated guest kernel). They are not bundled; if you register one as a
  docker runtime on the host, `container_runtime` can select it without code
  changes.
- The `exec` runner (`runtimes.<name>.runner: exec`) runs the agent directly
  on the host with **no container isolation at all**. Never use it for
  untrusted workloads.

Runtime selection is a per-worker deployment decision, not per-run: the
`Policy` proto (`proto/runtime/v1/runtime.proto`) deliberately has no
container-runtime field, so a client submitting a run cannot weaken a
hardened worker's isolation.

## Isolation selection matrix by threat tier

There is no single "best" sandbox; pick the layer whose isolation strength
matches the threat tier of the workload:

| Tier | Technology | Isolation boundary | Recommended for | nano-cloud status |
| --- | --- | --- | --- | --- |
| Highest | Firecracker microVM (or Kata) | Dedicated guest kernel in a KVM VM; a container escape still lands inside a disposable VM | Multi-tenant platforms, fully untrusted third-party agents | Not bundled; if registered as a docker runtime on the host, `container_runtime` selects it with no code change |
| Balanced (default) | gVisor / `runsc` | Userspace kernel intercepts syscalls; drastically shrinks the host-kernel attack surface | Production single-tenant runs of untrusted model output | **Supported and recommended**: `container_runtime: runsc` |
| Development | Docker + ECI (Docker Desktop Enhanced Container Isolation, Sysbox-based) or stock runc | Hardened userns container (ECI); plain runc shares the host kernel | Local development, trusted tasks, CI on your own code | Works out of the box; acceptable baseline only for trusted workloads |
| Lightweight | WebAssembly (Wasmtime/WAMR, browser sandbox) | Capability-based VM with no host access by default | Agents embedded in browsers or edge plugins | Out of scope for nano-cloud's OCI pipeline; relevant to browser-side nano-agent runtimes |

Threat-tier guidance for nano-cloud deployments: multi-tenant gateway
serving third parties → microVM tier; production runs of model-generated
code on your own fleet → `runsc` (the default recommendation);
developer laptops iterating on trusted tasks → Docker/ECI. Note the
operational trade-off used in the evaluation-backend positioning
(`README.md`): stronger sandboxes slow down filesystem and
dependency-install operations, which matters when hundreds of
time-boxed benchmark runs execute in parallel.

## Two design principles from harness-engineering practice

**(a) There is no perfect defense against prompt injection — limit the
blast radius.** The highest-value control is not detecting injection but
capping what a hijacked agent can do. A domain allowlist downgrades
"agent is hijacked" to "agent is hijacked *and can only reach allowlisted
hosts*". In nano-cloud this principle is load-bearing in two places: the
per-run `NETWORK_POLICY_ALLOWLIST` enforced by the worker's
`net-policy-proxy` sidecar (next section), and the worker-side-only
`container_runtime` setting, which a hijacked or malicious *client*
submitting runs through the gateway cannot weaken.

**(b) The next vulnerability class is not breaking out of the sandbox —
it is making the sandbox write something that a trusted process outside
will consume later.** The host-side *handoff surface* is the new
frontier, and every consumption path of worker-produced artifacts needs
explicit audit. In nano-cloud's architecture the trust boundary crossings
are:

- **Mounted workspace** (`workspace_root` / `host_workspace_root` in
  `pkg/worker/config.go`): files the agent writes into `/workspace`
  persist on the worker host disk. Any later process that reads, parses,
  or executes them (CI jobs, artifact collectors, an eval verifier)
  crosses the boundary.
- **Run logs on the host** (`openLogFiles` in `pkg/worker/worker.go`):
  `agent.stdout.log` / `agent.stderr.log` / `.nano-run.json` are written
  by the worker from agent output; tailing or tooling must treat them as
  untrusted text.
- **Event stream worker → gateway → console**: agent-controlled text
  (assistant deltas, status details) travels the WebSocket to the gateway
  and over SSE to the Console. The Console HTML-escapes event payloads
  before rendering (`htmlEscape` in `pkg/server/html_escape.go`) — an
  existing handoff-surface defense; any new consumer (dashboards, CLI
  renderers, webhook forwards) must do the same.

Audit rule of thumb: every path where worker output is consumed by a
trusted process should **escape on render, validate on parse, never
exec**. When adding a new consumer of run artifacts, list it here.

## Network policy matrix

`RunRequest.policy.network` (`enum NetworkPolicy`) maps to the following
worker behavior (`DockerExtraArgsFromPolicy` + the allowlist branch of
`handleRunRequest`):

| Policy | Docker wiring | Effective egress |
| --- | --- | --- |
| `NETWORK_POLICY_NONE` | `--network none` | No network at all (loopback only). |
| `NETWORK_POLICY_ALLOWLIST` | Internal network `<container>-allow` + `net-policy-proxy` sidecar; agent gets `HTTP(S)_PROXY=http://proxy:3128` | Only hosts/CIDRs in the worker's `network_allowlist`, default TCP 443. |
| `NETWORK_POLICY_ALL` | no extra flags | Full docker default (bridge/NAT) egress. |
| `NETWORK_POLICY_UNSPECIFIED` | no extra flags | Same as `ALL` — **the default is open**. |

Resource limits from `Policy.resources` are orthogonal and always applied
when present: `--cpus`, `--memory`, `--pids-limit`.

### How ALLOWLIST relates to net-policy-proxy

`ALLOWLIST` is enforced by the `net-policy-proxy` component
(`cmd/net-policy-proxy`, image `network_policy_image`, default
`nano-net-policy-runtime:local`, built from `docker/net-policy-runtime`):

1. The worker creates a **docker internal network** (no outbound route).
2. It starts the proxy on that network and additionally connects the proxy
   to the default `bridge` network, so only the proxy can reach the internet.
3. The agent container joins only the internal network; its
   `HTTP_PROXY`/`HTTPS_PROXY` (and lowercase variants) point at the proxy.
4. The proxy reads `network_allowlist` (mounted as
   `/etc/nano/allowlist.json`) and allows CONNECT/HTTP requests only to
   listed hosts (exact or `*.` wildcard) or CIDRs, TCP only, ports default
   to 443. Rules are validated by `ValidateNetworkAllowlist`
   (`pkg/worker/net_allowlist.go`); an empty or invalid allowlist fails the
   run with `NETWORK_ALLOWLIST_EMPTY` / `NETWORK_ALLOWLIST_INVALID` instead
   of silently opening egress.

Caveats to understand before relying on it:

- Enforcement combines a network-level block (internal network) with an
  application-level filter (proxy env vars). A process that ignores
  `HTTP(S)_PROXY` cannot reach out directly — the internal network has no
  route — but it can still reach the proxy, so keep the allowlist tight.
- UDP/DNS exfiltration through the proxy's DNS resolution is a known class
  of weakness for proxy-based allowlists; if you need to prevent DNS-tunnel
  exfiltration, prefer `NETWORK_POLICY_NONE` or an egress firewall at the
  host level.
- The proxy is a per-run sidecar and is torn down with the run (cancel and
  completion paths in `handleRunRequest`/`handleCancel`).

## Credentials: keep them out of containers, keep them short-lived

How credentials currently flow (all real mechanisms):

- **`env_passthrough`** (worker config): named host environment variables
  are copied into every agent container. This is the main way LLM API keys
  reach agents — anything listed here is visible to untrusted code inside
  the container. Keep the list minimal; never pass cloud-provider keys
  (AWS/GCP), SSH agent variables, or docker credentials.
- **`runtimes.<name>.env` / `env_file`** (worker config): static env for a
  runtime. `env_file` (`NanoAgentAdapter` in `pkg/worker/runtime/nano_agent.go`)
  keeps values on the worker host disk instead of the YAML, but they still
  end up as container env vars.
- **`agent-config.yaml`** generated by `worker quickstart` references keys
  as `${ENV_VAR}` placeholders rather than embedding them; the actual value
  still arrives via `env_passthrough`.
- **`secrets_ref`** exists in the `RunRequest` proto as a wire-level
  placeholder but is **not implemented** by the worker today — do not assume
  secret injection happens.

Recommendations for untrusted workloads:

1. Issue **short-lived, scope-limited LLM tokens** (e.g. proxy-issued keys
   with rate/spend limits) instead of long-lived master API keys in
   `env_passthrough`. A leaked container then only leaks a token that
   expires quickly and can be revoked.
2. Prefer `NETWORK_POLICY_ALLOWLIST` with only your LLM endpoint listed, so
   a leaked key can at least not be exfiltrated to an arbitrary host.
3. Worker↔Gateway auth: workers connect with `token`
   (`Authorization: Bearer` on the WebSocket, `pkg/worker/worker.go`). New
   workers are enrolled through Console-approved **pairing codes that expire
   after 15 minutes** (`PairingTTL` in `pkg/server/pairing_store.go`) — use
   the pairing flow rather than distributing a shared long-lived token, and
   rotate `token` when a worker host is decommissioned.

## Layering with the nano-agent sandbox

Isolation is defense in depth across two independent layers:

1. **Outer layer — nano-cloud worker (this repo).** Container isolation
   (runc, or `runsc`/MicroVM via `container_runtime`), network policy
   (matrix above), and resource limits. This boundary is enforced by the
   docker daemon / kernel / gVisor and is independent of what the agent
   process does.
2. **Inner layer — nano-agent (sibling repo, runs inside the container).**
   nano-agent sandboxes individual tool executions: bubblewrap (`bwrap`)
   profiles on Linux and `sandbox-exec` on macOS (see `pkg/sandbox` in the
   nano-agent repo), plus an approval policy (`Policy.approval`:
   `auto`/`ask`/`deny`) propagated to the container as the
   `APPROVAL_POLICY` env var (`pkg/worker/runtime/base.go`).

The layers are complementary: the inner sandbox restricts what agent *tool
calls* can touch even if the agent framework is tricked (prompt injection),
while the outer container boundary contains a full compromise of the agent
process itself. Note that inside the Linux runtime images only nano-agent's
bwrap path is relevant; `sandbox-exec` applies when nano-agent runs directly
on a macOS host. For untrusted code, enable both: `container_runtime: runsc`
outside, bwrap + `ask`/`deny` approval inside.

## Hardening checklist

- [ ] Linux worker host with `runsc` registered (`docker info` → Runtimes);
      `container_runtime: runsc` in `worker-config.yaml`.
- [ ] Untrusted runs submitted with `NETWORK_POLICY_NONE`, or
      `NETWORK_POLICY_ALLOWLIST` + a tight `network_allowlist`.
- [ ] `env_passthrough` trimmed to the minimum; short-lived scoped LLM
      tokens; no cloud/SSH credentials in the list.
- [ ] Worker enrolled via pairing code; `token` not shared across hosts.
- [ ] No `runner: exec` for untrusted runtimes.
- [ ] Resource limits (`Policy.resources`) set by the gateway/client to cap
      CPU/memory/pids per run.
- [ ] Isolation tier chosen from the selection matrix above matches the
      threat tier (multi-tenant → microVM; production untrusted → `runsc`).
- [ ] Every consumer of worker artifacts (workspace files, host logs,
      event stream) reviewed against the handoff-surface rule: escape on
      render, validate on parse, never exec.
