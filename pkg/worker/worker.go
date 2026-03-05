package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

type Worker struct {
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

func New(cfg *Config) *Worker {
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

func (w *Worker) Start(ctx context.Context) error {
	w.cfgMu.Lock()
	if w.cfg.WorkerID == "" {
		w.cfg.WorkerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if w.cfg.Name == "" {
		w.cfg.Name = fmt.Sprintf("nano-sandbox-%s", w.cfg.WorkerID)
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

	conn, err := w.dial(ctx)
	if err != nil {
		return err
	}
	w.conn = conn
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
		_ = conn.Close()
		return err
	}

	go w.heartbeatLoop(ctx)
	if cfg.StateDir != "" {
		go w.remoteConfigLoop(ctx)
	}

	for {
		env, err := w.readEnvelope()
		if err != nil {
			return err
		}
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
	workspaceDir := filepath.Join(cfg.WorkspaceRoot, sanitize(workspaceID))
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

	prompt := extractPrompt(req.GetInput())
	env := map[string]string{
		"STREAM_ID":  streamID,
		"SESSION_ID": req.SessionId,
		"PROMPT":     prompt,
	}
	if pol := req.GetPolicy(); pol != nil {
		if pol.TimeoutSeconds > 0 {
			env["TIMEOUT_SECONDS"] = fmt.Sprintf("%d", pol.TimeoutSeconds)
		}
		switch pol.Approval {
		case runtimev1.ApprovalPolicy_APPROVAL_POLICY_AUTO:
			env["APPROVAL_POLICY"] = "auto"
		case runtimev1.ApprovalPolicy_APPROVAL_POLICY_DENY:
			env["APPROVAL_POLICY"] = "deny"
		case runtimev1.ApprovalPolicy_APPROVAL_POLICY_ASK:
			env["APPROVAL_POLICY"] = "ask"
		}
	}
	for k, v := range runtimeCfg.Env {
		env[k] = v
	}
	for _, k := range cfg.EnvPassthrough {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}

	runner := strings.ToLower(strings.TrimSpace(runtimeCfg.Runner))
	if runner == "" {
		runner = "docker"
	}

	cmd := runtimeCfg.Command

	mounts := map[string]string{
		workspaceDir: "/workspace",
	}
	if cfg.AgentConfigPath != "" {
		absPath, err := filepath.Abs(cfg.AgentConfigPath)
		if err == nil {
			mounts[absPath] = "/root/.config/nano/config.yaml"
		} else {
			w.logger.Warnf("failed to resolve agent config path: %v", err)
		}
	}

	containerName := fmt.Sprintf("%s-%s", sanitize(cfg.WorkerID), sanitize(streamID))
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
			Mounts:  map[string]string{allowlistHostPath: "/etc/nano/allowlist.json"},
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

func extractPrompt(in *runtimev1.Input) string {
	if in == nil {
		return ""
	}
	for i := len(in.Messages) - 1; i >= 0; i-- {
		m := in.Messages[i]
		if m.Role == runtimev1.Role_ROLE_USER {
			return m.Content
		}
	}
	if len(in.Messages) > 0 {
		return in.Messages[len(in.Messages)-1].Content
	}
	return ""
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, ".", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "x"
	}
	return out
}
