package main //nolint:revive

import (
	"bufio"
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
	"gopkg.in/yaml.v2"
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
		case "config":
			handleConfigSubcommand(cfg, *configPath, args[1:])
			return
		case "quickstart":
			runQuickstart()
			return
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

		bootstrapCfg, err := worker.BootstrapFromRemote(ctx, boot)
		if err != nil {
			logrus.Fatalf("bootstrap failed: %v", err)
		}

		if cfg != nil {
			// Merge bootstrap result (worker ID, token, relay URL) into local config
			cfg.WorkerID = bootstrapCfg.WorkerID
			cfg.Token = bootstrapCfg.Token
			cfg.StateDir = bootstrapCfg.StateDir
			if actualRelayURL != "" {
				// An explicit relay was provided for bootstrap; ensure consistency.
				if cfg.RelayURL != "" && cfg.RelayURL != actualRelayURL {
					logrus.Fatalf("relay URL mismatch: config specifies %q but -relay/RELAY_URL provided %q", cfg.RelayURL, actualRelayURL)
				}
				cfg.RelayURL = bootstrapCfg.RelayURL
			} else if cfg.RelayURL == "" {
				cfg.RelayURL = bootstrapCfg.RelayURL
			}
		} else {
			cfg = bootstrapCfg
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

const defaultWorkerConfigPath = "worker-config.yaml"
const defaultAgentConfigPath = "agent-config.yaml"

func handleConfigSubcommand(cfg *worker.Config, cfgPath string, args []string) {
	if len(args) == 0 {
		printConfigUsage()
		os.Exit(1)
	}

	if cfgPath == "" {
		cfgPath = defaultWorkerConfigPath
	}

	switch args[0] {
	case "init":
		configInit(cfgPath)
	case "show":
		configShow(cfg, cfgPath)
	case "set":
		if len(args) < 3 {
			logrus.Fatal("config set requires <key> <value>")
		}
		configSet(cfg, cfgPath, args[1], args[2])
	case "agent-init":
		agentPath := defaultAgentConfigPath
		if len(args) >= 2 {
			agentPath = args[1]
		}
		configAgentInit(agentPath)
	case "agent-show":
		agentPath := defaultAgentConfigPath
		if cfg != nil && cfg.AgentConfigPath != "" {
			agentPath = cfg.AgentConfigPath
		}
		if len(args) >= 2 {
			agentPath = args[1]
		}
		configAgentShow(agentPath)
	case "agent-set":
		if len(args) < 3 {
			logrus.Fatal("config agent-set requires <key> <value>")
		}
		agentPath := defaultAgentConfigPath
		if cfg != nil && cfg.AgentConfigPath != "" {
			agentPath = cfg.AgentConfigPath
		}
		configAgentSet(agentPath, args[1], args[2])
	default:
		logrus.Fatalf("unknown config subcommand: %s", args[0])
	}
}

func printConfigUsage() {
	fmt.Println("Usage: worker config <subcommand> [args]")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  init                   Create default worker config file")
	fmt.Println("  show                   Show current worker config")
	fmt.Println("  set <key> <value>      Set a worker config field")
	fmt.Println("  agent-init [path]      Create default agent config file")
	fmt.Println("  agent-show [path]      Show current agent config")
	fmt.Println("  agent-set <key> <val>  Set agent config field (dot-notation key)")
	fmt.Println()
	fmt.Println("Tip: Use 'worker quickstart' for an interactive setup wizard.")
	fmt.Println()
	fmt.Println("Supported keys for 'set':")
	fmt.Println("  relay_url, name, version, workspace_root, labels, log_root,")
	fmt.Println("  agent_config_path, env_passthrough, host_workspace_root,")
	fmt.Println("  host_state_root, network_policy_image")
}

func configInit(cfgPath string) {
	if _, err := os.Stat(cfgPath); err == nil {
		logrus.Fatalf("config file already exists: %s", cfgPath)
	}
	cfg := worker.DefaultConfig()
	if err := worker.SaveConfig(cfgPath, cfg); err != nil {
		logrus.Fatalf("failed to save config: %v", err)
	}
	fmt.Printf("Worker config created: %s\n", cfgPath)
}

func configShow(cfg *worker.Config, cfgPath string) {
	if cfg == nil {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			logrus.Fatalf("cannot read config file %s: %v", cfgPath, err)
		}
		fmt.Print(string(b))
		return
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		logrus.Fatalf("failed to marshal config: %v", err)
	}
	fmt.Print(string(b))
}

func configSet(cfg *worker.Config, cfgPath string, key, value string) {
	if cfg == nil {
		var err error
		cfg, err = worker.LoadConfig(cfgPath)
		if err != nil {
			logrus.Fatalf("cannot load config file %s: %v", cfgPath, err)
		}
	}
	if err := worker.ConfigSet(cfg, key, value); err != nil {
		logrus.Fatalf("config set failed: %v", err)
	}
	if err := worker.SaveConfig(cfgPath, cfg); err != nil {
		logrus.Fatalf("failed to save config: %v", err)
	}
	fmt.Printf("Set %s = %s in %s\n", key, value, cfgPath)
}

func configAgentInit(agentPath string) {
	if _, err := os.Stat(agentPath); err == nil {
		logrus.Fatalf("agent config file already exists: %s", agentPath)
	}
	content := worker.DefaultAgentConfigYAML()
	if err := os.WriteFile(agentPath, []byte(content), 0o600); err != nil {
		logrus.Fatalf("failed to save agent config: %v", err)
	}
	fmt.Printf("Agent config created: %s\n", agentPath)
}

func configAgentShow(agentPath string) {
	b, err := os.ReadFile(agentPath)
	if err != nil {
		logrus.Fatalf("cannot read agent config file %s: %v", agentPath, err)
	}
	fmt.Print(string(b))
}

func configAgentSet(agentPath, key, value string) {
	b, err := os.ReadFile(agentPath)
	if err != nil {
		logrus.Fatalf("cannot read agent config file %s: %v", agentPath, err)
	}

	var data yaml.MapSlice
	if err := yaml.Unmarshal(b, &data); err != nil {
		logrus.Fatalf("failed to parse agent config: %v", err)
	}

	parts := strings.Split(key, ".")
	data = setNestedYAML(data, parts, value)

	out, err := yaml.Marshal(data)
	if err != nil {
		logrus.Fatalf("failed to marshal agent config: %v", err)
	}
	if err := os.WriteFile(agentPath, out, 0o600); err != nil {
		logrus.Fatalf("failed to save agent config: %v", err)
	}
	fmt.Printf("Set %s = %s in %s\n", key, value, agentPath)
}

func setNestedYAML(data yaml.MapSlice, keys []string, value string) yaml.MapSlice {
	if len(keys) == 0 {
		return data
	}
	key := keys[0]
	for i, item := range data {
		if fmt.Sprintf("%v", item.Key) == key {
			if len(keys) == 1 {
				data[i].Value = value
				return data
			}
			child, ok := item.Value.(yaml.MapSlice)
			if !ok {
				child = yaml.MapSlice{}
			}
			data[i].Value = setNestedYAML(child, keys[1:], value)
			return data
		}
	}
	// Key not found, create it
	if len(keys) == 1 {
		data = append(data, yaml.MapItem{Key: key, Value: value})
	} else {
		child := setNestedYAML(yaml.MapSlice{}, keys[1:], value)
		data = append(data, yaml.MapItem{Key: key, Value: child})
	}
	return data
}

func runQuickstart() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("🚀 Nano Worker Quick Setup")
	fmt.Println("─────────────────────────────")
	fmt.Println()

	// Step 1: Choose Agent
	fmt.Println("Which agent do you want to configure?")
	fmt.Println("  [1] nano-agent")
	fmt.Println("  [2] cli-agent (opencode/claude-code)")
	fmt.Println()
	agentChoice := promptInput(reader, "Agent", "1")
	isNanoAgent := agentChoice != "2"
	fmt.Printf("  → %s\n\n", map[bool]string{true: "nano-agent", false: "cli-agent"}[isNanoAgent])

	// Step 2: Choose provider
	providers := worker.Providers()
	fmt.Println("Which LLM provider do you want to use?")
	for i, p := range providers {
		fmt.Printf("  [%d] %s\n", i+1, p.Name)
	}
	fmt.Println()

	choice := promptInput(reader, "Provider", "1")
	idx := 0
	if n := parseChoice(choice, len(providers)); n >= 0 {
		idx = n
	}
	preset := providers[idx]
	if isNanoAgent {
		preset = worker.WithNanoAgent(preset)
	}
	fmt.Printf("  → %s\n\n", preset.Name)

	// Step 2: API Key (if needed)
	apiKey := ""
	if preset.NeedsAPIKey {
		envVal := os.Getenv(preset.EnvKey)
		hint := ""
		if envVal != "" {
			hint = maskKey(envVal)
		}
		apiKey = promptInput(reader, fmt.Sprintf("API Key (%s)", preset.EnvKey), hint)
		if apiKey == "" || apiKey == hint {
			apiKey = envVal
		}
	}

	// Step 3: Model
	model := promptInput(reader, "Model", preset.Model)

	// Step 4: Base URL (for custom/ollama)
	baseURL := ""
	if preset.Name == "Custom (OpenAI-compatible)" {
		for {
			baseURL = promptInput(reader, "Base URL", preset.DefaultURL)
			if strings.TrimSpace(baseURL) != "" {
				break
			}
			fmt.Println("Base URL is required for the selected provider (e.g. https://api.example.com/v1).")
		}
	} else if preset.Name == "Ollama (local)" {
		baseURL = promptInput(reader, "Base URL", preset.DefaultURL)
	}

	// Step 5: Gateway URL
	relayURL := promptInput(reader, "Gateway URL", "ws://localhost:8081")

	fmt.Println()

	// Build configs
	result, err := worker.BuildQuickstart(preset, apiKey, model, baseURL, relayURL)
	if err != nil {
		logrus.Fatalf("quickstart failed: %v", err)
	}

	// Write worker config
	workerPath := defaultWorkerConfigPath
	if _, err := os.Stat(workerPath); err == nil {
		overwrite := promptInput(reader, fmt.Sprintf("%s already exists. Overwrite? (y/N)", workerPath), "N")
		if !strings.HasPrefix(strings.ToLower(overwrite), "y") {
			fmt.Println("Aborted.")
			return
		}
	}
	if err := worker.SaveConfig(workerPath, result.WorkerConfig); err != nil {
		logrus.Fatalf("failed to save worker config: %v", err)
	}
	fmt.Printf("✅ Created %s\n", workerPath)

	// Write agent config
	agentPath := defaultAgentConfigPath
	if _, err := os.Stat(agentPath); err == nil {
		overwrite := promptInput(reader, fmt.Sprintf("%s already exists. Overwrite? (y/N)", agentPath), "N")
		if !strings.HasPrefix(strings.ToLower(overwrite), "y") {
			fmt.Printf("   Skipped %s (kept existing)\n", agentPath)
			fmt.Println()
			fmt.Printf("✅ Ready! Start with: worker -config %s\n", workerPath)
			return
		}
	}
	if err := os.WriteFile(agentPath, []byte(result.AgentConfigStr), 0o600); err != nil {
		logrus.Fatalf("failed to save agent config: %v", err)
	}
	fmt.Printf("✅ Created %s\n", agentPath)

	fmt.Println()
	fmt.Printf("✅ Ready! Start with: worker -config %s\n", workerPath)
}

func promptInput(reader *bufio.Reader, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal
	}
	return line
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

func parseChoice(s string, maxVal int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > maxVal {
		return 0
	}
	return n - 1
}
