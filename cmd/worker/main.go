package main //nolint:revive

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nano-harness/nano-cloud/pkg/worker"
	_ "github.com/joho/godotenv/autoload"
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
	logRoot := flag.String("log-root", "", "Centralized log directory for agent logs")
	labels := flag.String("labels", "", "Comma-separated labels (optional)")
	verbose := flag.Bool("verbose", false, "Verbose output for diagnose/logs")
	jsonOut := flag.Bool("json", false, "JSON output for diagnose")
	flag.Parse()

	actualRelayURL := *relayURL
	if actualRelayURL == "" {
		actualRelayURL = os.Getenv("RELAY_URL")
	}

	if *verbose {
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		for _, arg := range os.Args[1:] {
			if arg == "--verbose" || arg == "-verbose" {
				logrus.SetLevel(logrus.DebugLevel)
				*verbose = true
				break
			}
		}
	}

	logrus.SetOutput(os.Stdout)

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
			diagnoseWorker(cfg, actualRelayURL, *stateDir, *workspaceRoot, *logRoot, *verbose, *jsonOut)
			return
		case "troubleshoot":
			if len(args) < 2 {
				logrus.Fatal("troubleshoot requires a run_id")
			}
			troubleshootRun(cfg, actualRelayURL, *stateDir, *workspaceRoot, *logRoot, args[1], *verbose)
			return
		case "inspect":
			if len(args) < 2 {
				logrus.Fatal("inspect requires a run_id")
			}
			inspectRun(args[1])
			return
		case "logs":
			if len(args) < 2 {
				logrus.Fatal("logs requires a run_id")
			}
			runID := args[1]
			useStderr := false
			follow := false
			for _, a := range args[2:] {
				if a == "--stderr" || a == "stderr" {
					useStderr = true
				}
				if a == "--follow" || a == "-f" {
					follow = true
				}
			}
			logsWorker(cfg, *workspaceRoot, *logRoot, runID, useStderr, follow)
			return
		case "run":
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
	} else if actualRelayURL != "" || *enrollToken != "" || *stateDir != "" {
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
			boot.LogRoot = cfg.LogRoot
			boot.Labels = cfg.Labels
			boot.WorkerID = cfg.WorkerID
		}
		if actualRelayURL != "" {
			boot.RelayURL = actualRelayURL
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
		if *logRoot != "" {
			boot.LogRoot = *logRoot
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
	if *logRoot != "" {
		cfg.LogRoot = *logRoot
	}

	w := worker.New(cfg)

	if err := w.Start(ctx); err != nil {
		logrus.Fatalf("worker exited: %v", err)
	}
}

func diagnoseWorker(cfg *worker.Config, relayURL string, flagStateDir string, workspaceRoot string, logRoot string, verbose bool, jsonOut bool) {
	worker.CheckDockerInfo(context.Background())
	finalRelay := strings.TrimSpace(relayURL)
	if cfg != nil && strings.TrimSpace(cfg.RelayURL) != "" {
		finalRelay = strings.TrimSpace(cfg.RelayURL)
	}
	if finalRelay == "" {
		finalRelay = "ws://localhost:8081"
	}
	stateDir := strings.TrimSpace(flagStateDir)
	if stateDir == "" && cfg != nil {
		stateDir = strings.TrimSpace(cfg.StateDir)
	}
	worker.PrintGatewayDiagnostics(context.Background(), finalRelay, stateDir, verbose, jsonOut)

	if !jsonOut {
		resolvedLogRoot := strings.TrimSpace(logRoot)
		if resolvedLogRoot == "" && cfg != nil {
			resolvedLogRoot = strings.TrimSpace(cfg.LogRoot)
		}
		resolvedWorkspaceRoot := strings.TrimSpace(workspaceRoot)
		if resolvedWorkspaceRoot == "" && cfg != nil {
			resolvedWorkspaceRoot = strings.TrimSpace(cfg.WorkspaceRoot)
		}
		worker.PrintAgentLogsDiagnostics(resolvedLogRoot, resolvedWorkspaceRoot, verbose)
	}
}

func inspectRun(runID string) {
	fmt.Printf("Inspecting run %s...\n", runID)
	fmt.Printf("Run ID: %s\n", runID)
	fmt.Printf("Note: To view live logs, use:\n")
	fmt.Printf("  curl -NsS \"http://localhost:8081/v1/runs/%s/events\" -H \"Authorization: Bearer <your-token>\"\n", runID)
}

func logsWorker(cfg *worker.Config, flagWorkspaceRoot string, flagLogRoot string, runID string, useStderr bool, follow bool) {
	file := "agent.stdout.log"
	if useStderr {
		file = "agent.stderr.log"
	}

	// Sanitize runID to prevent path traversal
	cleanID := filepath.Base(filepath.Clean(runID))
	if cleanID == "." || cleanID == ".." || cleanID == "" {
		logrus.Fatalf("invalid run_id: %s", runID)
	}

	// Try log root directory first (centralized logs)
	lr := strings.TrimSpace(flagLogRoot)
	if lr == "" && cfg != nil {
		lr = strings.TrimSpace(cfg.LogRoot)
	}
	if lr != "" {
		logPath := filepath.Join(lr, cleanID, file)
		if _, err := os.Stat(logPath); err == nil {
			readLogFile(logPath, follow)
			return
		}
	}

	// Fall back to workspace root
	root := strings.TrimSpace(flagWorkspaceRoot)
	if root == "" && cfg != nil && strings.TrimSpace(cfg.WorkspaceRoot) != "" {
		root = strings.TrimSpace(cfg.WorkspaceRoot)
	}
	if root == "" {
		root = os.TempDir() + string(os.PathSeparator) + "nano-workspaces"
	}
	workspaceID := runID
	metaFound := ""
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta := filepath.Join(root, e.Name(), ".nano-workspace.json")
		if b, err := os.ReadFile(meta); err == nil {
			type meta struct {
				RunID       string `json:"run_id"`
				WorkspaceID string `json:"workspace_id"`
			}
			var m meta
			if err := json.Unmarshal(b, &m); err == nil {
				if m.RunID == runID {
					workspaceID = m.WorkspaceID
					metaFound = e.Name()
					break
				}
			}
		}
	}
	dir := filepath.Join(root, workspaceID)
	if metaFound != "" {
		dir = filepath.Join(root, metaFound)
	}
	path := filepath.Join(dir, file)
	readLogFile(path, follow)
}

func readLogFile(path string, follow bool) {
	f, err := os.Open(path)
	if err != nil {
		logrus.Fatalf("open log: %v", err)
	}
	defer func() { _ = f.Close() }()
	if !follow {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			logrus.Fatalf("read log: %v", err)
		}
		return
	}
	info, err := f.Stat()
	if err != nil {
		logrus.Fatalf("stat log: %v", err)
	}
	offset := info.Size()
	for {
		time.Sleep(500 * time.Millisecond)
		nf, err := os.Open(path)
		if err != nil {
			continue
		}
		ninfo, err := nf.Stat()
		if err != nil {
			_ = nf.Close()
			continue
		}
		if ninfo.Size() > offset {
			_, _ = nf.Seek(offset, 0)
			_, _ = io.Copy(os.Stdout, nf)
			offset = ninfo.Size()
		}
		_ = nf.Close()
	}
}

func troubleshootRun(cfg *worker.Config, relayURL string, stateDir string, workspaceRoot string, logRoot string, runID string, verbose bool) {
	fmt.Println("=== Troubleshoot ===")
	finalRelay := strings.TrimSpace(relayURL)
	if cfg != nil && strings.TrimSpace(cfg.RelayURL) != "" {
		finalRelay = strings.TrimSpace(cfg.RelayURL)
	}
	if finalRelay == "" {
		finalRelay = "ws://localhost:8081"
	}
	fmt.Println("Relay:", finalRelay)
	fmt.Println("Run ID:", runID)
	fmt.Println("SSE:")
	base := "http://localhost:8081"
	if strings.HasPrefix(finalRelay, "wss://") {
		base = "https://" + strings.TrimPrefix(finalRelay, "wss://")
	} else if strings.HasPrefix(finalRelay, "ws://") {
		base = "http://" + strings.TrimPrefix(finalRelay, "ws://")
	}
	fmt.Printf("  curl -NsS \"%s/v1/runs/%s/events\" -H \"Authorization: Bearer <token>\"\n", base, runID)
	fmt.Println("Logs:")
	fmt.Printf("  worker logs %s --follow\n", runID)
	fmt.Println("\n-- Diagnostics --")
	worker.PrintGatewayDiagnostics(context.Background(), finalRelay, stateDir, verbose, false)
	fmt.Println("\n-- Logs Tail (stdout) --")
	logsWorker(cfg, workspaceRoot, logRoot, runID, false, false)
	fmt.Println("\n-- Logs Tail (stderr) --")
	logsWorker(cfg, workspaceRoot, logRoot, runID, true, false)
}
