package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"gopkg.in/yaml.v2"
)

type fileConfig struct {
	CLI map[string]cliRuntimeConfig `yaml:"cli"`
}

type cliRuntimeConfig struct {
	Env  map[string]string `yaml:"env"`
	Args []string          `yaml:"args"`
}

func main() {
	bin := strings.TrimSpace(os.Getenv("RUNTIME_BIN"))
	prompt := strings.TrimSpace(os.Getenv("PROMPT"))
	if bin == "" || prompt == "" {
		os.Exit(2)
	}

	runtimeKey := strings.TrimSpace(os.Getenv("NANO_RUNTIME_KEY"))
	if runtimeKey == "" {
		runtimeKey = runtimeKeyFromBin(bin)
	}

	cfgPath := strings.TrimSpace(os.Getenv("NANO_CONFIG_PATH"))
	if cfgPath == "" {
		cfgPath = "/root/.config/nano/config.yaml"
	}

	entry, _ := loadCLIRuntimeConfig(cfgPath, runtimeKey)
	applyEnv(entry.Env)

	args := []string{}
	if v := strings.TrimSpace(os.Getenv("RUNTIME_ARGS")); v != "" {
		args = append(args, strings.Fields(v)...)
	}
	for _, a := range entry.Args {
		a = os.ExpandEnv(a)
		if strings.TrimSpace(a) != "" {
			args = append(args, a)
		}
	}
	args = append(args, prompt)

	target, err := exec.LookPath(bin)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(127)
	}
	if err := syscall.Exec(target, append([]string{bin}, args...), os.Environ()); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(126)
	}
}

func runtimeKeyFromBin(bin string) string {
	b := strings.ToLower(strings.TrimSpace(bin))
	switch b {
	case "claude":
		return "claude_code"
	case "opencode":
		return "opencode"
	default:
		return b
	}
}

func loadCLIRuntimeConfig(path string, runtimeKey string) (cliRuntimeConfig, error) {
	if runtimeKey == "" {
		return cliRuntimeConfig{}, errors.New("missing runtime key")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cliRuntimeConfig{}, err
	}
	var cfg fileConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cliRuntimeConfig{}, err
	}
	if cfg.CLI == nil {
		return cliRuntimeConfig{}, errors.New("missing cli section")
	}
	entry, ok := cfg.CLI[runtimeKey]
	if !ok {
		return cliRuntimeConfig{}, errors.New("runtime not found")
	}
	return entry, nil
}

func applyEnv(env map[string]string) {
	for k, v := range env {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v = os.ExpandEnv(v)
		_ = os.Setenv(k, v)
	}
}
