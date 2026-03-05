package main

import (
	"context"
	"flag"
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

	w := worker.New(cfg)

	if err := w.Start(ctx); err != nil {
		logrus.Fatalf("worker exited: %v", err)
	}
}
