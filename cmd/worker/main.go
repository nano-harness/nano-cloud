package main //nolint:revive

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nano-harness/nano-cloud/pkg/worker"
	"github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("config", "", "Worker config file (YAML)")
	relayURL := flag.String("relay", "", "Relay URL (ws:// or wss://)")
	enrollToken := flag.String("enroll-token", "", "Enroll token (one-time or short-lived)")
	stateDir := flag.String("state-dir", "", "State directory for worker token/config cache")
	workspaceRoot := flag.String("workspace-root", "", "Workspace root directory")
	hostWorkspaceRoot := flag.String("host-workspace-root", "", "Workspace root directory on the Docker host (for sibling container mounts)")
	hostStateRoot := flag.String("host-state-root", "", "State directory on the Docker host (for sibling container mounts)")
	labels := flag.String("labels", "", "Comma-separated labels (optional)")
	flag.Parse()

	var cfg *worker.Config
	var err error
	if *configPath != "" {
		cfg, err = worker.LoadConfig(*configPath)
		if err != nil {
			logrus.Fatalf("failed to load config: %v", err)
		}
	}

	parseLabels := func(s string) []string {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	// Check subcommand
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "diagnose":
			// Implement diagnose subcommand
			diagnoseWorker(cfg, *relayURL)
			return
		case "inspect":
			// Implement inspect subcommand
			if len(args) < 2 {
				logrus.Fatal("inspect requires a run_id")
			}
			inspectRun(args[1])
			return
		case "run":
			// Explicit run subcommand, do nothing and proceed
		default:
			if !strings.HasPrefix(args[0], "-") {
				logrus.Fatalf("unknown subcommand: %s", args[0])
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()

	useRemote := false
	if cfg == nil {
		useRemote = true
	} else if cfg.EnrollToken != "" || cfg.StateDir != "" {
		useRemote = true
	} else if *relayURL != "" || *enrollToken != "" || *stateDir != "" {
		useRemote = true
	}

	if useRemote {
		boot := worker.BootstrapConfig{}
		if cfg != nil {
			boot.RelayURL = cfg.RelayURL
			boot.EnrollToken = cfg.EnrollToken
			boot.WorkspaceRoot = cfg.WorkspaceRoot
			boot.HostWorkspaceRoot = cfg.HostWorkspaceRoot
			boot.HostStateRoot = cfg.HostStateRoot
			boot.StateDir = cfg.StateDir
			boot.Labels = cfg.Labels
			boot.WorkerID = cfg.WorkerID
		}
		if *relayURL != "" {
			boot.RelayURL = *relayURL
		}
		if *enrollToken != "" {
			boot.EnrollToken = *enrollToken
		}
		if *stateDir != "" {
			boot.StateDir = *stateDir
		}
		if *workspaceRoot != "" {
			boot.WorkspaceRoot = *workspaceRoot
		}
		if *hostWorkspaceRoot != "" {
			boot.HostWorkspaceRoot = *hostWorkspaceRoot
		}
		if *hostStateRoot != "" {
			boot.HostStateRoot = *hostStateRoot
		}
		if ls := parseLabels(*labels); len(ls) > 0 {
			boot.Labels = ls
		}

		cfg, err = worker.BootstrapFromRemote(ctx, boot)
		if err != nil {
			logrus.Fatalf("bootstrap failed: %v", err)
		}
	} else if cfg == nil {
		logrus.Fatal("missing -config or (-relay + -enroll-token)")
	}

	if *hostWorkspaceRoot != "" {
		cfg.HostWorkspaceRoot = *hostWorkspaceRoot
	}
	if *hostStateRoot != "" {
		cfg.HostStateRoot = *hostStateRoot
	}

	w := worker.New(cfg)

	if err := w.Start(ctx); err != nil {
		logrus.Fatalf("worker exited: %v", err)
	}
}

func diagnoseWorker(cfg *worker.Config, relayURL string) {
	// Simple pre-checks
	worker.CheckDockerInfo(context.Background())

	fmt.Printf("\n--- Network Check ---\n")
	if cfg != nil && cfg.RelayURL != "" {
		fmt.Printf("Relay URL (from config): %s\n", cfg.RelayURL)
	} else if relayURL != "" {
		fmt.Printf("Relay URL (from flag): %s\n", relayURL)
	} else {
		fmt.Printf("Relay URL: NOT SET\n")
	}

	fmt.Printf("HTTP_PROXY: %s\n", os.Getenv("HTTP_PROXY"))
	fmt.Printf("HTTPS_PROXY: %s\n", os.Getenv("HTTPS_PROXY"))
	fmt.Printf("NO_PROXY: %s\n", os.Getenv("NO_PROXY"))

	fmt.Printf("---------------------\n")
}

func inspectRun(runID string) {
	fmt.Printf("Inspecting run %s...\n", runID)
	// Usually we need gateway token to fetch SSE, which might be in state.
	// For local CLI, we can print where the user can find logs.
	fmt.Printf("Run ID: %s\n", runID)
	fmt.Printf("Note: To view live logs, use:\n")
	fmt.Printf("  curl -NsS \"http://localhost:8081/v1/runs/%s/events\" -H \"Authorization: Bearer <your-token>\"\n", runID)
}
