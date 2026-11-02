package worker

import (
	"strings"
	"testing"
)

func findProvider(name string) ProviderPreset {
	for _, p := range Providers() {
		if p.Name == name {
			return p
		}
	}
	panic("provider not found: " + name)
}

func TestProviders(t *testing.T) {
	providers := Providers()
	if len(providers) < 4 {
		t.Fatalf("expected at least 4 providers, got %d", len(providers))
	}
	names := map[string]bool{}
	for _, p := range providers {
		if p.Name == "" {
			t.Fatal("provider name is empty")
		}
		if names[p.Name] {
			t.Fatalf("duplicate provider name: %s", p.Name)
		}
		names[p.Name] = true
		if p.Runtime == "" {
			t.Fatalf("provider %s has empty runtime", p.Name)
		}
	}
}

func TestBuildQuickstartAnthropic(t *testing.T) {
	preset := findProvider("Anthropic")
	result, err := BuildQuickstart(preset, "sk-ant-test123", "", "", "ws://gw:8081")
	if err != nil {
		t.Fatal(err)
	}

	if result.WorkerConfig.RelayURL != "ws://gw:8081" {
		t.Fatalf("expected relay ws://gw:8081, got %s", result.WorkerConfig.RelayURL)
	}
	if result.WorkerConfig.AgentConfigPath != "agent-config.yaml" {
		t.Fatalf("expected agent_config_path agent-config.yaml, got %s", result.WorkerConfig.AgentConfigPath)
	}
	if !strings.Contains(result.AgentConfigStr, "${ANTHROPIC_API_KEY}") {
		t.Fatal("expected agent config to reference ${ANTHROPIC_API_KEY}")
	}
	if !strings.Contains(result.AgentConfigStr, "claude-sonnet-4-20250514") {
		t.Fatal("expected agent config to contain default model")
	}
}

func TestBuildQuickstartOpenAI(t *testing.T) {
	preset := findProvider("OpenAI")
	result, err := BuildQuickstart(preset, "sk-openai-xxx", "gpt-4o-mini", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if result.WorkerConfig.RelayURL != "ws://localhost:8081" {
		t.Fatalf("expected default relay URL, got %s", result.WorkerConfig.RelayURL)
	}
	if !strings.Contains(result.AgentConfigStr, "${OPENAI_API_KEY}") {
		t.Fatal("expected agent config to reference ${OPENAI_API_KEY}")
	}
	if !strings.Contains(result.AgentConfigStr, "gpt-4o-mini") {
		t.Fatal("expected agent config to contain model override")
	}
}

func TestBuildQuickstartOllama(t *testing.T) {
	preset := findProvider("Ollama (local)")
	result, err := BuildQuickstart(preset, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.AgentConfigStr, "ollama") {
		t.Fatal("expected agent config to use ollama placeholder key")
	}
	if !strings.Contains(result.AgentConfigStr, "localhost:11434") {
		t.Fatal("expected agent config to contain Ollama URL")
	}
	if !strings.Contains(result.AgentConfigStr, "qwen2.5-coder:7b") {
		t.Fatal("expected agent config to contain default Ollama model")
	}
}

func TestBuildQuickstartCustomBaseURL(t *testing.T) {
	preset := findProvider("Custom (OpenAI-compatible)")
	result, err := BuildQuickstart(preset, "my-key", "my-model", "https://my-llm.example.com/v1", "")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.AgentConfigStr, "https://my-llm.example.com/v1") {
		t.Fatal("expected agent config to contain custom base URL")
	}
	if !strings.Contains(result.AgentConfigStr, "my-model") {
		t.Fatal("expected agent config to contain custom model")
	}
	if !strings.Contains(result.AgentConfigStr, "${OPENAI_API_KEY}") {
		t.Fatal("expected agent config to reference ${OPENAI_API_KEY}")
	}
}

func TestBuildQuickstartCustomEmptyBaseURL(t *testing.T) {
	preset := findProvider("Custom (OpenAI-compatible)")
	_, err := BuildQuickstart(preset, "my-key", "my-model", "", "")
	if err == nil {
		t.Fatal("expected error for custom provider with empty base URL")
	}
}

func TestBuildQuickstartEnvPassthrough(t *testing.T) {
	preset := findProvider("OpenAI")
	result, err := BuildQuickstart(preset, "test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, k := range result.WorkerConfig.EnvPassthrough {
		if k == "OPENAI_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected OPENAI_API_KEY in env_passthrough")
	}
}

func TestBuildQuickstartProxyEnvPassthrough(t *testing.T) {
	preset := findProvider("OpenAI")
	result, err := BuildQuickstart(preset, "test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	proxyVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	for _, pv := range proxyVars {
		found := false
		for _, k := range result.WorkerConfig.EnvPassthrough {
			if k == pv {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected %s in env_passthrough", pv)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RelayURL == "" {
		t.Fatal("expected default relay URL")
	}
	if len(cfg.Runtimes) == 0 {
		t.Fatal("expected default runtimes")
	}
	if len(cfg.EnvPassthrough) == 0 {
		t.Fatal("expected default env_passthrough")
	}
}

func TestDefaultAgentConfigYAML(t *testing.T) {
	yaml := DefaultAgentConfigYAML()
	if !strings.Contains(yaml, "claude_code") {
		t.Fatal("expected claude_code in default agent config")
	}
	if !strings.Contains(yaml, "opencode") {
		t.Fatal("expected opencode in default agent config")
	}
}

func TestConfigSet(t *testing.T) {
	cfg := DefaultConfig()

	if err := ConfigSet(cfg, "relay_url", "ws://test:9999"); err != nil {
		t.Fatal(err)
	}
	if cfg.RelayURL != "ws://test:9999" {
		t.Fatalf("expected ws://test:9999, got %s", cfg.RelayURL)
	}

	if err := ConfigSet(cfg, "labels", "gpu,fast,local"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Labels) != 3 || cfg.Labels[0] != "gpu" {
		t.Fatalf("unexpected labels: %v", cfg.Labels)
	}

	if err := ConfigSet(cfg, "unknown_key", "value"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}
