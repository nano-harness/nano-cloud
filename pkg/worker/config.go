package worker

import (
	"os"

	"gopkg.in/yaml.v2"
)

type RuntimeConfig struct { //nolint:revive
	Runner          string            `yaml:"runner,omitempty"`
	Image           string            `yaml:"image"`
	Command         []string          `yaml:"command,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	EnvFile         string            `yaml:"env_file,omitempty"`
	AgentConfigDest string            `yaml:"agent_config_dest,omitempty"`
}

type NetworkAllowRule struct { //nolint:revive
	Host     string `yaml:"host"`
	CIDR     string `yaml:"cidr"`
	Protocol string `yaml:"protocol"`
	Ports    []int  `yaml:"ports"`
}

type Config struct { //nolint:revive
	RelayURL           string                   `yaml:"relay_url"`
	EnrollToken        string                   `yaml:"enroll_token"`
	Token              string                   `yaml:"token"`
	WorkerID           string                   `yaml:"worker_id"`
	Name               string                   `yaml:"name"`
	Version            string                   `yaml:"version"`
	Labels             []string                 `yaml:"labels"`
	WorkspaceRoot      string                   `yaml:"workspace_root"`
	HostWorkspaceRoot  string                   `yaml:"host_workspace_root"`
	HostStateRoot      string                   `yaml:"host_state_root"`
	StateDir           string                   `yaml:"state_dir"`
	LogRoot            string                   `yaml:"log_root"`
	EnvPassthrough     []string                 `yaml:"env_passthrough"`
	AgentConfigPath    string                   `yaml:"agent_config_path"`
	Runtimes           map[string]RuntimeConfig `yaml:"runtimes"`
	NetworkPolicyImage string                   `yaml:"network_policy_image"`
	NetworkAllowlist   []NetworkAllowRule       `yaml:"network_allowlist"`
}

func LoadConfig(path string) (*Config, error) { //nolint:revive
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
