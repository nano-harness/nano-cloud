package worker

import (
	"os"

	"gopkg.in/yaml.v2"
)

type RuntimeConfig struct {
	Runner  string            `yaml:"runner"`
	Image   string            `yaml:"image"`
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

type NetworkAllowRule struct {
	Host     string `yaml:"host"`
	CIDR     string `yaml:"cidr"`
	Protocol string `yaml:"protocol"`
	Ports    []int  `yaml:"ports"`
}

type Config struct {
	RelayURL           string                   `yaml:"relay_url"`
	EnrollToken        string                   `yaml:"enroll_token"`
	Token              string                   `yaml:"token"`
	WorkerID           string                   `yaml:"worker_id"`
	Name               string                   `yaml:"name"`
	Version            string                   `yaml:"version"`
	Labels             []string                 `yaml:"labels"`
	WorkspaceRoot      string                   `yaml:"workspace_root"`
	StateDir           string                   `yaml:"state_dir"`
	EnvPassthrough     []string                 `yaml:"env_passthrough"`
	AgentConfigPath    string                   `yaml:"agent_config_path"`
	Runtimes           map[string]RuntimeConfig `yaml:"runtimes"`
	NetworkPolicyImage string                   `yaml:"network_policy_image"`
	NetworkAllowlist   []NetworkAllowRule       `yaml:"network_allowlist"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
