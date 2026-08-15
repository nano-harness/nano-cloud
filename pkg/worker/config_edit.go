package worker

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

// DefaultConfig returns a default worker configuration.
func DefaultConfig() *Config { //nolint:revive
	return &Config{
		RelayURL:      "ws://localhost:8081",
		Name:          "nano-worker",
		Version:       "2.0",
		Labels:        []string{"docker-desktop"},
		WorkspaceRoot: "/tmp/nano-workspaces",
		EnvPassthrough: []string{
			"NANO_API_KEY",
			"NANO_BASE_URL",
			"NANO_MODEL",
			"OPENAI_API_KEY",
			"ANTHROPIC_API_KEY",
			"HTTP_PROXY",
			"HTTPS_PROXY",
			"NO_PROXY",
			"http_proxy",
			"https_proxy",
			"no_proxy",
		},
		Runtimes: map[string]RuntimeConfig{
			"nano_agent": {
				Image:           "nano-agent-runtime:local",
				AgentConfigDest: "/root/.config/nano/config.yaml",
			},
			"claude_code": {
				Image: "nano-cli-runtime:local",
				Env:   map[string]string{"RUNTIME_BIN": "claude"},
			},
			"opencode": {
				Image: "nano-cli-runtime:local",
				Env:   map[string]string{"RUNTIME_BIN": "opencode"},
			},
			"custom": {
				Image: "nano-cli-runtime:local",
				Env:   map[string]string{"RUNTIME_BIN": "/bin/echo"},
			},
		},
	}
}

// DefaultAgentConfigYAML returns the default agent configuration YAML.
func DefaultAgentConfigYAML() string { //nolint:revive
	return `cli:
  claude_code:
    env:
      ANTHROPIC_API_KEY: "${ANTHROPIC_API_KEY}"
    args: ["--model", "claude-sonnet-4-20250514"]
  opencode:
    env:
      OPENAI_API_KEY: "${OPENAI_API_KEY}"
    args: ["--model", "gpt-4o-mini"]
`
}

// SaveConfig writes a Config to the given YAML file path.
func SaveConfig(path string, cfg *Config) error { //nolint:revive
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, b, 0o600)
}

// ProviderPreset describes an LLM provider preset for quickstart.
type ProviderPreset struct { //nolint:revive
	Name        string // display name
	EnvKey      string // primary API key env var name
	DefaultURL  string // default base URL (empty means provider default)
	Runtime     string // which CLI runtime to use (claude_code, opencode, nano_agent)
	Model       string // default model name
	NeedsAPIKey bool   // whether an API key is required
}

// Providers returns the built-in provider presets using cli-agent runtimes by default.
// When the user selects nano-agent mode in the quickstart, pass the returned preset through
// WithNanoAgent to switch the Runtime field accordingly.
func Providers() []ProviderPreset { //nolint:revive
	return []ProviderPreset{
		{
			Name:        "OpenAI",
			EnvKey:      "OPENAI_API_KEY",
			DefaultURL:  "https://api.openai.com/v1",
			Runtime:     "opencode",
			Model:       "gpt-4o",
			NeedsAPIKey: true,
		},
		{
			Name:        "Anthropic",
			EnvKey:      "ANTHROPIC_API_KEY",
			DefaultURL:  "https://api.anthropic.com",
			Runtime:     "claude_code",
			Model:       "claude-sonnet-4-20250514",
			NeedsAPIKey: true,
		},
		{
			Name:        "DeepSeek",
			EnvKey:      "DEEPSEEK_API_KEY",
			DefaultURL:  "https://api.deepseek.com/v1",
			Runtime:     "opencode",
			Model:       "deepseek-chat",
			NeedsAPIKey: true,
		},
		{
			Name:        "Ollama (local)",
			EnvKey:      "OPENAI_API_KEY",
			DefaultURL:  "http://localhost:11434/v1",
			Runtime:     "opencode",
			Model:       "qwen2.5-coder:7b",
			NeedsAPIKey: false,
		},
		{
			Name:        "Custom (OpenAI-compatible)",
			EnvKey:      "OPENAI_API_KEY",
			DefaultURL:  "",
			Runtime:     "opencode",
			Model:       "",
			NeedsAPIKey: true,
		},
	}
}

// WithNanoAgent returns a copy of the preset with Runtime set to "nano_agent".
func WithNanoAgent(p ProviderPreset) ProviderPreset { //nolint:revive
	p.Runtime = "nano_agent"
	return p
}

// QuickstartResult holds the output of the quickstart wizard.
type QuickstartResult struct { //nolint:revive
	WorkerConfig   *Config
	AgentConfigStr string
}

// BuildQuickstart generates a worker config and agent config YAML from the
// quickstart parameters: provider preset, API key, model override, base URL
// override, and gateway relay URL. API keys are referenced via ${ENV_VAR}
// placeholders in the agent config rather than embedded directly.
func BuildQuickstart(preset ProviderPreset, apiKey, model, baseURL, relayURL string) (QuickstartResult, error) { //nolint:revive
	if model == "" {
		model = preset.Model
	}
	if baseURL == "" {
		baseURL = preset.DefaultURL
	}

	// Custom providers must have a non-empty base URL to avoid broken configs.
	if baseURL == "" && preset.DefaultURL == "" {
		return QuickstartResult{}, fmt.Errorf("base URL must be provided for custom providers")
	}

	cfg := DefaultConfig()
	if relayURL != "" {
		cfg.RelayURL = relayURL
	}
	cfg.AgentConfigPath = defaultAgentConfigPath

	// Build agent config using ${ENV_VAR} references instead of raw keys.
	var agentYAML string
	switch preset.Runtime {
	case "nano_agent":
		envKey := preset.EnvKey
		apiKeyVal := ""
		if preset.NeedsAPIKey {
			apiKeyVal = fmt.Sprintf("${%s}", envKey)
		} else {
			apiKeyVal = "ollama"
		}
		agentYAML = fmt.Sprintf(`api_key: %q
base_url: %q
model: %q
verbose: true

# System Limits and Timeouts
max_file_size: 10485760  # 10MB in bytes
response_timeout: 300s   # LLM response timeout
http_timeout: 60s        # HTTP client timeout

# Context Management
context:
  max_tokens: 120000
  compression_ratio: 0.25
  preserve_recent_turns: 8
  enable_compression: true
`, apiKeyVal, baseURL, model)
	case "claude_code":
		agentYAML = fmt.Sprintf(`cli:
  claude_code:
    env:
      ANTHROPIC_API_KEY: "${ANTHROPIC_API_KEY}"
    args: ["--model", %q]
  opencode:
    env:
      OPENAI_API_KEY: "${OPENAI_API_KEY}"
    args: ["--model", "gpt-4o-mini"]
`, model)
	default: // opencode or custom
		envKey := preset.EnvKey
		if !preset.NeedsAPIKey {
			// Ollama doesn't need a real key; use a placeholder literal.
			agentYAML = fmt.Sprintf(`cli:
  claude_code:
    env:
      ANTHROPIC_API_KEY: "${ANTHROPIC_API_KEY}"
    args: ["--model", "claude-sonnet-4-20250514"]
  opencode:
    env:
      %s: "ollama"
      OPENAI_BASE_URL: %q
    args: ["--model", %q]
`, envKey, baseURL, model)
		} else {
			agentYAML = fmt.Sprintf(`cli:
  claude_code:
    env:
      ANTHROPIC_API_KEY: "${ANTHROPIC_API_KEY}"
    args: ["--model", "claude-sonnet-4-20250514"]
  opencode:
    env:
      %s: "${%s}"
      OPENAI_BASE_URL: %q
    args: ["--model", %q]
`, envKey, envKey, baseURL, model)
		}
	}

	// Add passthrough for NANO_* and the provider key
	seen := map[string]bool{}
	var envPass []string
	for _, k := range cfg.EnvPassthrough {
		if !seen[k] {
			seen[k] = true
			envPass = append(envPass, k)
		}
	}
	if !seen[preset.EnvKey] {
		envPass = append(envPass, preset.EnvKey)
	}
	cfg.EnvPassthrough = envPass

	return QuickstartResult{
		WorkerConfig:   cfg,
		AgentConfigStr: agentYAML,
	}, nil
}

const defaultAgentConfigPath = "agent-config.yaml"

// ConfigSet sets a top-level worker config field by key.
// Supported keys: relay_url, name, version, workspace_root, labels,
// log_root, agent_config_path, env_passthrough, host_workspace_root, host_state_root, network_policy_image.
func ConfigSet(cfg *Config, key string, value string) error { //nolint:revive
	switch key {
	case "relay_url":
		cfg.RelayURL = value
	case "name":
		cfg.Name = value
	case "version":
		cfg.Version = value
	case "workspace_root":
		cfg.WorkspaceRoot = value
	case "host_workspace_root":
		cfg.HostWorkspaceRoot = value
	case "host_state_root":
		cfg.HostStateRoot = value
	case "log_root":
		cfg.LogRoot = value
	case "agent_config_path":
		cfg.AgentConfigPath = value
	case "labels":
		parts := strings.Split(value, ",")
		labels := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				labels = append(labels, p)
			}
		}
		cfg.Labels = labels
	case "env_passthrough":
		parts := strings.Split(value, ",")
		envs := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				envs = append(envs, p)
			}
		}
		cfg.EnvPassthrough = envs
	case "network_policy_image":
		cfg.NetworkPolicyImage = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}
