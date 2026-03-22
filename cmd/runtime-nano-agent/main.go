package main //nolint:revive

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nano-harness/nano-agent/pkg/agent"
	"github.com/nano-harness/nano-agent/pkg/config"
	"github.com/nano-harness/nano-agent/pkg/event"
)

func main() {
	prompt := strings.TrimSpace(os.Getenv("PROMPT"))
	if prompt == "" {
		os.Exit(2)
	}

	cfg, err := config.LoadConfig("")
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}

	policy := strings.ToLower(strings.TrimSpace(os.Getenv("APPROVAL_POLICY")))
	approvalHandler := func(_ *agent.ToolCallInfo) bool {
		switch policy {
		case "auto", "allow", "true", "yes":
			return true
		default:
			return false
		}
	}

	a, err := agent.New(cfg, approvalHandler)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}

	timeoutSeconds := int64(300)
	if v := strings.TrimSpace(os.Getenv("TIMEOUT_SECONDS")); v != "" {
		if parsed, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil && parsed > 0 {
			timeoutSeconds = parsed
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	sessionID := strings.TrimSpace(os.Getenv("SESSION_ID"))
	if sessionID == "" {
		sessionID = "default"
	}

	// Persist sessions in the workspace to ensure isolation and persistence across runs
	a.GetSessionManager().SetStorage(agent.NewLocalSessionStorage("/workspace/.nano/sessions"))

	eventHandler := func(e event.StreamEvent) {
		if e.Content != "" {
			_, _ = os.Stdout.WriteString(e.Content)
		}
	}

	if err := a.ProcessStreamWithMultimodalAndSession(ctx, sessionID, prompt, nil, eventHandler); err != nil {
		os.Stderr.WriteString(err.Error() + "\n") //nolint:errcheck
		os.Exit(1)
	}
}
