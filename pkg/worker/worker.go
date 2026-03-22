package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nano-harness/nano-cloud/pkg/worker/runtime"
	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

type Worker struct { //nolint:revive
	cfgMu  sync.RWMutex
	cfg    *Config
	logger *logrus.Logger

	conn    *websocket.Conn
	writeMu sync.Mutex

	mu   sync.Mutex
	runs map[string]*runState
	seq  uint64
}

type runState struct {
	streamID string
	cancel   context.CancelFunc
	done     atomic.Bool
	proc     *DockerProcess
	netProc  *DockerProcess
	netName  string
	logs     *DockerLogStream
	cmd      *exec.Cmd
}

func New(cfg *Config) *Worker { //nolint:revive
	l := logrus.New()
	return &Worker{
		cfg:    cfg,
		logger: l,
		runs:   make(map[string]*runState),
	}
}

func (w *Worker) getConfig() *Config {
	w.cfgMu.RLock()
	defer w.cfgMu.RUnlock()
	return w.cfg
}

func (w *Worker) setConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	w.cfgMu.Lock()
	w.cfg = cfg
	w.cfgMu.Unlock()
}

func (w *Worker) nextSeq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	return w.seq
}

func (w *Worker) dial(ctx context.Context) (*websocket.Conn, error) {
	cfg := w.getConfig()
	u, err := url.Parse(cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	// Normalize port: default to 8081 when none is specified, consistent with
	// httpBaseURLFromRelayURL and normalize_relay_url in connect.sh.  Without
	// this, a portless relay URL would fall back to the scheme default (80 for
	// ws://, 443 for wss://) instead of the gateway's standard port, causing a
	// 502 error on the WebSocket handshake.
	if u.Port() == "" && u.Hostname() != "" {
		u.Host = net.JoinHostPort(u.Hostname(), "8081")
	}
	u.Path = "/v1/worker/connect"
	dialer := websocket.Dialer{}
	headers := http.Header{}
	if cfg.Token != "" {
		headers.Set("Authorization", "Bearer "+cfg.Token)
	}
	conn, _, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (w *Worker) Start(ctx context.Context) error { //nolint:revive
	w.cfgMu.Lock()
	if w.cfg.WorkerID == "" {
		w.cfg.WorkerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if w.cfg.Name == "" {
		w.cfg.Name = fmt.Sprintf("nano-sandbox-%s", runtime.Sanitize(w.cfg.WorkerID))
	}
	if w.cfg.Version == "" {
		w.cfg.Version = "1.0"
	}
	if w.cfg.WorkspaceRoot == "" {
		w.cfg.WorkspaceRoot = filepath.Join(os.TempDir(), "nano-workspaces")
	}
	workspaceRoot := w.cfg.WorkspaceRoot
	w.cfgMu.Unlock()

	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return err
	}

	if w.cfg.StateDir != "" {
		go w.remoteConfigLoop(ctx)
	}

	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := w.connectAndServe(ctx)
		if err == nil {
			return nil
		}
		w.logger.Errorf("Worker disconnected: %v. Reconnecting in %v...", err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (w *Worker) connectAndServe(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	conn, err := w.dial(ctx)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	w.conn = conn
	w.writeMu.Unlock()
	defer conn.Close() //nolint:errcheck

	conn.SetReadDeadline(time.Now().Add(pongWait))                                                         //nolint:errcheck
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(pongWait)); return nil }) //nolint:errcheck

	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.writeMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(writeWait)) //nolint:errcheck
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					w.writeMu.Unlock()
					return
				}
				w.writeMu.Unlock()
			}
		}
	}()

	cfg := w.getConfig()

	hello := &runtimev1.WorkerHello{
		Name:              cfg.Name,
		Version:           cfg.Version,
		Labels:            cfg.Labels,
		SupportedRuntimes: w.supportedRuntimes(),
		Capacity:          &runtimev1.Capacity{},
		Docker:            &runtimev1.DockerInfo{},
	}
	if err := w.writeEnvelope(&runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        cfg.WorkerID,
		Seq:             w.nextSeq(),
		Message:         &runtimev1.Envelope_WorkerHello{WorkerHello: hello},
	}); err != nil {
		return err
	}

	go w.heartbeatLoop(ctx)

	for {
		env, err := w.readEnvelope()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(pongWait)) //nolint:errcheck

		if req := env.GetRunRequest(); req != nil {
			streamID := env.StreamId
			if streamID == "" {
				streamID = fmt.Sprintf("run-%d", time.Now().UnixNano())
			}
			go w.handleRunRequest(ctx, streamID, req)
			continue
		}
		if cancel := env.GetCancelRequest(); cancel != nil {
			streamID := env.StreamId
			go w.handleCancel(streamID, cancel.Reason)
			continue
		}
	}
}

func (w *Worker) supportedRuntimes() []runtimev1.Runtime {
	cfg := w.getConfig()
	if len(cfg.Runtimes) == 0 {
		return []runtimev1.Runtime{runtimev1.Runtime_RUNTIME_CUSTOM}
	}

	normalizeKey := func(k string) string {
		k = strings.ToLower(strings.TrimSpace(k))
		k = strings.ReplaceAll(k, "-", "_")
		return k
	}

	available := make(map[string]struct{}, len(cfg.Runtimes))
	for k := range cfg.Runtimes {
		available[normalizeKey(k)] = struct{}{}
	}

	runtimes := make([]runtimev1.Runtime, 0, len(available))
	used := make(map[string]struct{}, 4)

	priority := []struct {
		key string
		rt  runtimev1.Runtime
	}{
		{key: "nano_agent", rt: runtimev1.Runtime_RUNTIME_NANO_AGENT},
		{key: "claude_code", rt: runtimev1.Runtime_RUNTIME_CLAUDE_CODE},
		{key: "opencode", rt: runtimev1.Runtime_RUNTIME_OPENCODE},
	}
	for _, p := range priority {
		if _, ok := available[p.key]; ok {
			runtimes = append(runtimes, p.rt)
			used[p.key] = struct{}{}
		}
	}

	unknownKeys := make([]string, 0, len(available))
	for k := range available {
		if _, ok := used[k]; ok {
			continue
		}
		unknownKeys = append(unknownKeys, k)
	}
	sort.Strings(unknownKeys)
	if len(unknownKeys) > 0 {
		runtimes = append(runtimes, runtimev1.Runtime_RUNTIME_CUSTOM)
	}

	if len(runtimes) == 0 {
		return []runtimev1.Runtime{runtimev1.Runtime_RUNTIME_CUSTOM}
	}
	return runtimes
}

func resolveHostBindPath(hostRoot string, containerRoot string, containerPath string) (string, error) {
	if hostRoot == "" || containerRoot == "" || containerPath == "" {
		return "", nil
	}
	rel, err := filepath.Rel(containerRoot, containerPath)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("path %s is outside container root %s", containerPath, containerRoot)
	}
	return filepath.Join(hostRoot, rel), nil
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cfg := w.getConfig()
			_ = w.writeEnvelope(&runtimev1.Envelope{
				ProtocolVersion: "1.0",
				WorkerId:        cfg.WorkerID,
				Seq:             w.nextSeq(),
				Message: &runtimev1.Envelope_WorkerHeartbeat{WorkerHeartbeat: &runtimev1.WorkerHeartbeat{
					UnixMillis: uint64(time.Now().UnixMilli()),
					Current:    &runtimev1.Capacity{},
				}},
			})
		}
	}
}

func (w *Worker) handleCancel(streamID string, reason string) {
	w.mu.Lock()
	rs := w.runs[streamID]
	w.mu.Unlock()
	if rs == nil {
		return
	}
	if !rs.done.CompareAndSwap(false, true) {
		return
	}
	if rs.cancel != nil {
		rs.cancel()
	}
	if rs.logs != nil {
		_ = rs.logs.Stop()
	}
	if rs.proc != nil {
		_ = rs.proc.Stop(context.Background())
		target := rs.proc.ContainerID
		if target == "" {
			target = rs.proc.Container
		}
		_ = DockerRemove(context.Background(), target)
	}
	if rs.netProc != nil {
		_ = rs.netProc.Stop(context.Background())
		target := rs.netProc.ContainerID
		if target == "" {
			target = rs.netProc.Container
		}
		_ = DockerRemove(context.Background(), target)
	}
	if rs.netName != "" {
		_ = DockerNetworkRemove(context.Background(), rs.netName)
	}
	if rs.cmd != nil && rs.cmd.Process != nil {
		_ = rs.cmd.Process.Kill()
	}
	_ = w.sendStatus(streamID, "cancelled", reason)
	_ = w.sendCompleted(streamID, false, 137, `{"reason":"cancelled"}`)
}

func (w *Worker) handleRunRequest(parent context.Context, streamID string, req *runtimev1.RunRequest) {
	ctx, cancel := context.WithCancel(parent)
	rs := &runState{streamID: streamID, cancel: cancel}
	w.mu.Lock()
	w.runs[streamID] = rs
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.runs, streamID)
		w.mu.Unlock()
	}()

	if ctx.Err() != nil || rs.done.Load() {
		return
	}

	_ = w.sendStatus(streamID, "starting", "")

	cfg := w.getConfig()

	workspaceID := req.GetWorkspace().GetWorkspaceId()
	if workspaceID == "" {
		workspaceID = streamID
	}
	workspaceDir := filepath.Join(cfg.WorkspaceRoot, runtime.Sanitize(workspaceID))
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		_ = w.sendError(streamID, "WORKSPACE_CREATE_FAILED", err.Error(), "")
		_ = w.sendCompleted(streamID, false, 1, "")
		return
	}

	runtimeKey := strings.ToLower(strings.TrimPrefix(req.Runtime.String(), "RUNTIME_"))
	runtimeCfg, ok := cfg.Runtimes[runtimeKey]
	if !ok {
		runtimeCfg, ok = cfg.Runtimes[strings.ReplaceAll(runtimeKey, "_", "-")]
	}
	if !ok {
		_ = w.sendError(streamID, "RUNTIME_NOT_CONFIGURED", fmt.Sprintf("runtime=%s", runtimeKey), "")
		if rs.done.CompareAndSwap(false, true) {
			_ = w.sendCompleted(streamID, false, 2, "")
		}
		return
	}

	// Runtime Adapter Selection
	var adapter runtime.RuntimeAdapter
	baseAdapter := runtime.BaseAdapter{
		Image:   runtimeCfg.Image,
		Command: runtimeCfg.Command,
		Env:     runtimeCfg.Env,
	}

	// Add EnvPassthrough to base adapter env
	for _, k := range cfg.EnvPassthrough {
		if v := os.Getenv(k); v != "" {
			if baseAdapter.Env == nil {
				baseAdapter.Env = make(map[string]string)
			}
			baseAdapter.Env[k] = v
		}
	}

	if runtimeKey == "nano_agent" {
		adapter = &runtime.NanoAgentAdapter{
			BaseAdapter: baseAdapter,
			EnvFile:     runtimeCfg.EnvFile,
		}
	} else {
		adapter = &baseAdapter
	}

	var runHostWorkspacePath string
	if cfg.HostWorkspaceRoot != "" {
		hostPath, hostErr := resolveHostBindPath(cfg.HostWorkspaceRoot, cfg.WorkspaceRoot, workspaceDir)
		if hostErr == nil {
			runHostWorkspacePath = hostPath
		} else {
			w.logger.Warnf("failed to resolve host path for workspace: %v", hostErr)
		}
	}

	runHostAgentConfigPath := cfg.AgentConfigPath
	if cfg.HostStateRoot != "" && cfg.StateDir != "" && cfg.AgentConfigPath != "" {
		hostCfgPath, hostErr := resolveHostBindPath(cfg.HostStateRoot, cfg.StateDir, cfg.AgentConfigPath)
		if hostErr == nil {
			runHostAgentConfigPath = hostCfgPath
		} else {
			w.logger.Warnf("failed to resolve host path for agent config: %v", hostErr)
		}
	}

	spec, err := adapter.BuildSpec(ctx, req, runtime.ContextInfo{
		StreamID:          streamID,
		WorkspaceDir:      workspaceDir,
		HostWorkspacePath: runHostWorkspacePath,
		WorkerID:          cfg.WorkerID,
		AgentConfigPath:   runHostAgentConfigPath,
	})
	if err != nil {
		_ = w.sendError(streamID, "RUNTIME_SPEC_BUILD_FAILED", err.Error(), "")
		if rs.done.CompareAndSwap(false, true) {
			_ = w.sendCompleted(streamID, false, 2, "")
		}
		return
	}

	runner := strings.ToLower(strings.TrimSpace(runtimeCfg.Runner))
	if runner == "" {
		runner = "docker"
	}

	containerName := fmt.Sprintf("%s-%s", runtime.Sanitize(cfg.WorkerID), runtime.Sanitize(streamID))
	env := spec.Env
	cmd := spec.Command
	mounts := spec.Mounts

	if runner == "exec" {
		if len(cmd) == 0 {
			_ = w.sendError(streamID, "EXEC_COMMAND_EMPTY", fmt.Sprintf("runtime=%s", runtimeKey), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}

		if ctx.Err() != nil || rs.done.Load() {
			return
		}

		localCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
		localCmd.Dir = workspaceDir
		localCmd.Env = append([]string(nil), os.Environ()...)
		for k, v := range env {
			localCmd.Env = append(localCmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
		stdout, err := localCmd.StdoutPipe()
		if err != nil {
			_ = w.sendError(streamID, "EXEC_STDOUT_PIPE_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}
		stderr, err := localCmd.StderrPipe()
		if err != nil {
			_ = w.sendError(streamID, "EXEC_STDERR_PIPE_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}
		if err := localCmd.Start(); err != nil {
			if ctx.Err() != nil || rs.done.Load() || errors.Is(err, context.Canceled) {
				return
			}
			_ = w.sendError(streamID, "EXEC_START_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}
		rs.cmd = localCmd

		_ = w.sendStatus(streamID, "running", "exec")

		scan := func(r io.Reader, isStderr bool) {
			s := bufio.NewScanner(r)
			s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for s.Scan() {
				line := s.Text()
				if isStderr {
					_ = w.sendError(streamID, "STDERR", line, "")
					continue
				}
				_ = w.sendAssistantDelta(streamID, line+"\n")
			}
			if err := s.Err(); err != nil {
				_ = w.sendError(streamID, "STDERR", fmt.Sprintf("scan failed: %v", err), "")
			}
		}
		go scan(stdout, false)
		go scan(stderr, true)

		err = localCmd.Wait()
		exitCode := 0
		if localCmd.ProcessState != nil {
			exitCode = localCmd.ProcessState.ExitCode()
		}
		if err != nil && exitCode == 0 {
			exitCode = 1
		}
		if rs.done.CompareAndSwap(false, true) {
			success := exitCode == 0
			_ = w.sendCompleted(streamID, success, int32(exitCode), "")
		}
		return
	}

	if runtimeCfg.Image == "" {
		_ = w.sendError(streamID, "RUNTIME_IMAGE_EMPTY", fmt.Sprintf("runtime=%s", runtimeKey), "")
		if rs.done.CompareAndSwap(false, true) {
			_ = w.sendCompleted(streamID, false, 2, "")
		}
		return
	}

	pol := req.GetPolicy()
	if pol != nil && pol.Network == runtimev1.NetworkPolicy_NETWORK_POLICY_ALLOWLIST {
		cfg := w.getConfig()
		if len(cfg.NetworkAllowlist) == 0 {
			_ = w.sendError(streamID, "NETWORK_ALLOWLIST_EMPTY", "no allowlist rules configured", "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 2, "")
			}
			return
		}
		if err := ValidateNetworkAllowlist(cfg.NetworkAllowlist); err != nil {
			_ = w.sendError(streamID, "NETWORK_ALLOWLIST_INVALID", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 2, "")
			}
			return
		}
		policyImage := strings.TrimSpace(cfg.NetworkPolicyImage)
		if policyImage == "" {
			policyImage = "nano-net-policy-runtime:local"
		}
		netName := containerName + "-allow"
		if err := DockerNetworkCreateInternal(ctx, netName); err != nil {
			_ = w.sendError(streamID, "DOCKER_NETWORK_CREATE_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}
		rs.netName = netName

		allowlistHostPath := filepath.Join(workspaceDir, ".nano-allowlist.json")
		allowlistMountPath := allowlistHostPath
		if cfg.HostWorkspaceRoot != "" {
			if rel, relErr := filepath.Rel(cfg.WorkspaceRoot, workspaceDir); relErr == nil {
				allowlistMountPath = filepath.Join(cfg.HostWorkspaceRoot, rel, ".nano-allowlist.json")
			} else {
				w.logger.Warnf("failed to resolve host path for allowlist: %v", relErr)
			}
		}
		b, err := json.Marshal(cfg.NetworkAllowlist)
		if err != nil {
			_ = w.sendError(streamID, "NETWORK_ALLOWLIST_INVALID", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 2, "")
			}
			return
		}
		if err := os.WriteFile(allowlistHostPath, b, 0o644); err != nil {
			_ = w.sendError(streamID, "WORKSPACE_WRITE_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 1, "")
			}
			return
		}

		proxyName := containerName + "-proxy"
		proxyAlias := "proxy"
		proxyProc, err := DockerRun(ctx, DockerRunSpec{
			Image:   policyImage,
			Name:    proxyName,
			Detach:  true,
			Remove:  false,
			Mounts:  map[string]string{allowlistMountPath: "/etc/nano/allowlist.json"},
			Command: []string{"-listen", ":3128", "-allowlist", "/etc/nano/allowlist.json"},
			ExtraArgs: []string{
				"--network", netName,
				"--network-alias", proxyAlias,
			},
		})
		rs.netProc = proxyProc
		if err != nil || rs.netProc == nil {
			msg := "docker run returned nil"
			if err != nil {
				msg = err.Error()
			}
			_ = DockerNetworkRemove(context.Background(), netName)
			_ = w.sendError(streamID, "DOCKER_NET_POLICY_FAILED", msg, "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}
		if err := DockerNetworkConnect(ctx, "bridge", proxyName); err != nil {
			_ = DockerRemove(context.Background(), proxyName)
			_ = DockerNetworkRemove(context.Background(), netName)
			_ = w.sendError(streamID, "DOCKER_NET_POLICY_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}

		proxyURL := fmt.Sprintf("http://%s:3128", proxyAlias)
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["NO_PROXY"] = "localhost,127.0.0.1"
		env["no_proxy"] = "localhost,127.0.0.1"

		extraArgs := DockerExtraArgsFromPolicy(pol)
		extraArgs = append(extraArgs, "--network", netName)

		proc, err := DockerRun(ctx, DockerRunSpec{
			Image:     runtimeCfg.Image,
			Name:      containerName,
			Workdir:   "/workspace",
			Mounts:    mounts,
			Env:       env,
			Command:   cmd,
			Detach:    true,
			Remove:    false,
			ExtraArgs: extraArgs,
		})
		rs.proc = proc
		if err != nil || rs.proc == nil {
			if ctx.Err() != nil || rs.done.Load() || errors.Is(err, context.Canceled) {
				return
			}
			msg := "docker run returned nil"
			if err != nil {
				msg = err.Error()
			}
			_ = DockerRemove(context.Background(), proxyName)
			_ = DockerNetworkRemove(context.Background(), netName)
			_ = w.sendError(streamID, "DOCKER_RUN_FAILED", msg, "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 3, "")
			}
			return
		}

		_ = w.sendStatus(streamID, "running", containerName)

		rs.logs, _ = DockerLogsFollow(ctx, containerName, func(isStderr bool, line string) {
			if isStderr {
				_ = w.sendError(streamID, "STDERR", line, "")
				return
			}
			_ = w.sendAssistantDelta(streamID, line+"\n")
		})

		exitCode, err := DockerWait(ctx, containerName)
		if err != nil {
			if ctx.Err() != nil || rs.done.Load() || errors.Is(err, context.Canceled) {
				return
			}
			_ = DockerRemove(context.Background(), proxyName)
			_ = DockerNetworkRemove(context.Background(), netName)
			_ = w.sendError(streamID, "DOCKER_WAIT_FAILED", err.Error(), "")
			if rs.done.CompareAndSwap(false, true) {
				_ = w.sendCompleted(streamID, false, 4, "")
			}
			return
		}
		_ = DockerRemove(context.Background(), containerName)
		if rs.netProc != nil {
			_ = DockerRemove(context.Background(), proxyName)
		}
		_ = DockerNetworkRemove(context.Background(), netName)
		if rs.done.CompareAndSwap(false, true) {
			success := exitCode == 0
			_ = w.sendCompleted(streamID, success, int32(exitCode), "")
		}
		return
	}

	extraArgs := DockerExtraArgsFromPolicy(pol)

	proc, err := DockerRun(ctx, DockerRunSpec{
		Image:     runtimeCfg.Image,
		Name:      containerName,
		Workdir:   "/workspace",
		Mounts:    mounts,
		Env:       env,
		Command:   cmd,
		Detach:    true,
		Remove:    false,
		ExtraArgs: extraArgs,
	})
	rs.proc = proc
	if err != nil || rs.proc == nil {
		if ctx.Err() != nil || rs.done.Load() || errors.Is(err, context.Canceled) {
			return
		}
		msg := "docker run returned nil"
		if err != nil {
			msg = err.Error()
		}
		_ = w.sendError(streamID, "DOCKER_RUN_FAILED", msg, "")
		if rs.done.CompareAndSwap(false, true) {
			_ = w.sendCompleted(streamID, false, 3, "")
		}
		return
	}

	_ = w.sendStatus(streamID, "running", containerName)

	rs.logs, _ = DockerLogsFollow(ctx, containerName, func(isStderr bool, line string) {
		if isStderr {
			_ = w.sendError(streamID, "STDERR", line, "")
			return
		}
		_ = w.sendAssistantDelta(streamID, line+"\n")
	})

	exitCode, err := DockerWait(ctx, containerName)
	if err != nil {
		if ctx.Err() != nil || rs.done.Load() || errors.Is(err, context.Canceled) {
			return
		}
		_ = w.sendError(streamID, "DOCKER_WAIT_FAILED", err.Error(), "")
		if rs.done.CompareAndSwap(false, true) {
			_ = w.sendCompleted(streamID, false, 4, "")
		}
		return
	}
	_ = DockerRemove(context.Background(), containerName)
	if rs.done.CompareAndSwap(false, true) {
		success := exitCode == 0
		_ = w.sendCompleted(streamID, success, int32(exitCode), "")
	}
}

func (w *Worker) sendStatus(streamID string, status string, detail string) error {
	w.logger.WithFields(logrus.Fields{
		"stream_id": streamID,
		"status":    status,
		"detail":    detail,
	}).Info("run status")
	return w.sendRunEvent(streamID, &runtimev1.RunEvent{
		Kind:    runtimev1.EventKind_EVENT_KIND_STATUS,
		Payload: &runtimev1.RunEvent_Status{Status: &runtimev1.StatusEvent{Status: status, Detail: detail}},
	})
}

func (w *Worker) sendAssistantDelta(streamID string, text string) error {
	return w.sendRunEvent(streamID, &runtimev1.RunEvent{
		Kind:    runtimev1.EventKind_EVENT_KIND_ASSISTANT_DELTA,
		Payload: &runtimev1.RunEvent_AssistantDelta{AssistantDelta: &runtimev1.AssistantDeltaEvent{Text: text}},
	})
}

func (w *Worker) sendCompleted(streamID string, success bool, exitCode int32, statsJSON string) error {
	w.logger.WithFields(logrus.Fields{
		"stream_id": streamID,
		"success":   success,
		"exit_code": exitCode,
	}).Info("run completed")
	return w.sendRunEvent(streamID, &runtimev1.RunEvent{
		Kind: runtimev1.EventKind_EVENT_KIND_COMPLETED,
		Payload: &runtimev1.RunEvent_Completed{Completed: &runtimev1.CompletedEvent{
			Success:   success,
			ExitCode:  exitCode,
			StatsJson: statsJSON,
		}},
	})
}

func (w *Worker) sendError(streamID string, code string, message string, detailsJSON string) error {
	w.logger.WithFields(logrus.Fields{
		"stream_id": streamID,
		"code":      code,
		"message":   message,
	}).Error("run error")
	return w.sendRunEvent(streamID, &runtimev1.RunEvent{
		Kind: runtimev1.EventKind_EVENT_KIND_ERROR,
		Payload: &runtimev1.RunEvent_Error{Error: &runtimev1.Error{
			Code:        code,
			Message:     message,
			DetailsJson: detailsJSON,
		}},
	})
}

func (w *Worker) sendRunEvent(streamID string, evt *runtimev1.RunEvent) error {
	cfg := w.getConfig()
	return w.writeEnvelope(&runtimev1.Envelope{
		ProtocolVersion: "1.0",
		WorkerId:        cfg.WorkerID,
		StreamId:        streamID,
		Seq:             w.nextSeq(),
		Message:         &runtimev1.Envelope_RunEvent{RunEvent: evt},
	})
}

func (w *Worker) writeEnvelope(env *runtimev1.Envelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (w *Worker) readEnvelope() (*runtimev1.Envelope, error) {
	msgType, data, err := w.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if msgType != websocket.BinaryMessage {
		return nil, fmt.Errorf("unexpected websocket message type: %d", msgType)
	}
	var env runtimev1.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
