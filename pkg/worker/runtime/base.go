package runtime //nolint:revive

import (
	"context"
	"fmt"
	"strings"

	runtimev1 "github.com/nano-harness/nano-cloud/proto/runtime/v1"
)

type BaseAdapter struct { //nolint:revive
	Image   string
	Command []string
	Env     map[string]string
}

func (a *BaseAdapter) Name() string { //nolint:revive
	return "base"
}

func (a *BaseAdapter) BuildSpec(ctx context.Context, req *runtimev1.RunRequest, info ContextInfo) (*DockerRunSpec, error) { //nolint:revive
	if a.Image == "" {
		return nil, fmt.Errorf("runtime image is required")
	}

	env := make(map[string]string)
	for k, v := range a.Env {
		env[k] = v
	}

	// Basic Environment Variables
	env["STREAM_ID"] = info.StreamID
	env["SESSION_ID"] = req.SessionId
	env["PROMPT"] = extractPrompt(req.GetInput())

	// Policy
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

	mounts := map[string]string{
		info.WorkspaceDir: "/workspace",
	}
	if info.HostWorkspacePath != "" {
		mounts[info.HostWorkspacePath] = "/workspace"
		delete(mounts, info.WorkspaceDir)
	}

	if info.AgentConfigPath != "" {
		mounts[info.AgentConfigPath] = "/root/.config/nano/config.yaml"
	}

	return &DockerRunSpec{
		Image:   a.Image,
		Name:    fmt.Sprintf("%s-%s", Sanitize(info.WorkerID), Sanitize(info.StreamID)),
		Workdir: "/workspace",
		Mounts:  mounts,
		Env:     env,
		Command: a.Command,
		Detach:  true,
		Remove:  false,
	}, nil
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

func Sanitize(s string) string { //nolint:revive
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
