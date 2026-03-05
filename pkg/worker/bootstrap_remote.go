package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type BootstrapConfig struct {
	RelayURL      string
	EnrollToken   string
	WorkspaceRoot string
	StateDir      string
	Labels        []string
	WorkerID      string
}

type bootstrapState struct {
	WorkerID      string `json:"worker_id"`
	WorkerToken   string `json:"worker_token"`
	ConfigVersion string `json:"config_version"`
}

type enrollRequest struct {
	EnrollToken string   `json:"enroll_token"`
	WorkerID    string   `json:"worker_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

type enrollResponse struct {
	WorkerID      string `json:"worker_id"`
	WorkerToken   string `json:"worker_token"`
	ConfigVersion string `json:"config_version"`
}

type getConfigResponse struct {
	WorkerConfigYAML string `json:"worker_config_yaml"`
	AgentConfigYAML  string `json:"agent_config_yaml,omitempty"`
	ConfigVersion    string `json:"config_version"`
}

func BootstrapFromRemote(ctx context.Context, boot BootstrapConfig) (*Config, error) {
	if boot.RelayURL == "" {
		return nil, errors.New("relay_url is required")
	}
	stateDir := boot.StateDir
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			home = os.TempDir()
		}
		stateDir = filepath.Join(home, ".nano-cloud", "state")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	boot.StateDir = stateDir

	baseURL, err := httpBaseURLFromRelayURL(boot.RelayURL)
	if err != nil {
		return nil, err
	}

	statePath := filepath.Join(stateDir, "state.json")
	workerCfgPath := filepath.Join(stateDir, "worker-config.yaml")
	agentCfgPath := filepath.Join(stateDir, "agent-config.yaml")

	st, _ := readBootstrapState(statePath)
	if st.WorkerID == "" {
		st.WorkerID = boot.WorkerID
	}

	client := &http.Client{Timeout: 15 * time.Second}

	fetchConfig := func() (*getConfigResponse, bool, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/worker/config", nil)
		if err != nil {
			return nil, false, 0, err
		}
		if st.WorkerToken != "" {
			req.Header.Set("Authorization", "Bearer "+st.WorkerToken)
		}
		if st.ConfigVersion != "" {
			req.Header.Set("If-None-Match", `"`+st.ConfigVersion+`"`)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, false, 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotModified {
			return nil, true, resp.StatusCode, nil
		}
		if resp.StatusCode != http.StatusOK {
			return nil, false, resp.StatusCode, nil
		}
		var out getConfigResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, false, resp.StatusCode, err
		}
		return &out, false, resp.StatusCode, nil
	}

	enroll := func() error {
		if boot.EnrollToken == "" {
			return errors.New("enroll_token is required for first bootstrap")
		}
		body, _ := json.Marshal(enrollRequest{
			EnrollToken: boot.EnrollToken,
			WorkerID:    st.WorkerID,
			Labels:      boot.Labels,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/worker/enroll", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("enroll failed: status=%d", resp.StatusCode)
		}
		var out enrollResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		st.WorkerID = out.WorkerID
		st.WorkerToken = out.WorkerToken
		st.ConfigVersion = ""
		return writeBootstrapState(statePath, st)
	}

	if st.WorkerToken != "" {
		cfgResp, notModified, status, err := fetchConfig()
		if err != nil {
			return nil, err
		}
		switch status {
		case http.StatusOK:
			if cfgResp == nil {
				return nil, errors.New("empty config response")
			}
			if err := os.WriteFile(workerCfgPath, []byte(cfgResp.WorkerConfigYAML), 0o600); err != nil {
				return nil, err
			}
			if cfgResp.AgentConfigYAML != "" {
				if err := os.WriteFile(agentCfgPath, []byte(cfgResp.AgentConfigYAML), 0o600); err != nil {
					return nil, err
				}
			}
			st.ConfigVersion = cfgResp.ConfigVersion
			_ = writeBootstrapState(statePath, st)
			return parseAndMergeWorkerConfig(cfgResp.WorkerConfigYAML, st, boot, agentCfgPath)
		case http.StatusNotModified:
			if !notModified {
				return nil, errors.New("unexpected not modified state")
			}
			wb, err := os.ReadFile(workerCfgPath)
			if err == nil {
				return parseAndMergeWorkerConfig(string(wb), st, boot, agentCfgPath)
			}
			st.ConfigVersion = ""
			_ = writeBootstrapState(statePath, st)
			cfgResp, _, status, err = fetchConfig()
			if err != nil {
				return nil, err
			}
			if status != http.StatusOK || cfgResp == nil {
				return nil, fmt.Errorf("get config failed after cache miss: status=%d", status)
			}
			if err := os.WriteFile(workerCfgPath, []byte(cfgResp.WorkerConfigYAML), 0o600); err != nil {
				return nil, err
			}
			if cfgResp.AgentConfigYAML != "" {
				if err := os.WriteFile(agentCfgPath, []byte(cfgResp.AgentConfigYAML), 0o600); err != nil {
					return nil, err
				}
			}
			st.ConfigVersion = cfgResp.ConfigVersion
			_ = writeBootstrapState(statePath, st)
			return parseAndMergeWorkerConfig(cfgResp.WorkerConfigYAML, st, boot, agentCfgPath)
		case http.StatusUnauthorized:
			st.WorkerToken = ""
			st.ConfigVersion = ""
			_ = writeBootstrapState(statePath, st)
		default:
			return nil, fmt.Errorf("get config failed: status=%d", status)
		}
	}

	if err := enroll(); err != nil {
		return nil, err
	}
	cfgResp, _, status, err := fetchConfig()
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || cfgResp == nil {
		return nil, fmt.Errorf("get config failed: status=%d", status)
	}
	if err := os.WriteFile(workerCfgPath, []byte(cfgResp.WorkerConfigYAML), 0o600); err != nil {
		return nil, err
	}
	if cfgResp.AgentConfigYAML != "" {
		if err := os.WriteFile(agentCfgPath, []byte(cfgResp.AgentConfigYAML), 0o600); err != nil {
			return nil, err
		}
	}
	st.ConfigVersion = cfgResp.ConfigVersion
	_ = writeBootstrapState(statePath, st)

	return parseAndMergeWorkerConfig(cfgResp.WorkerConfigYAML, st, boot, agentCfgPath)
}

func parseAndMergeWorkerConfig(workerConfigYAML string, st bootstrapState, boot BootstrapConfig, agentCfgPath string) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(workerConfigYAML), &cfg); err != nil {
		return nil, err
	}

	if cfg.RelayURL == "" {
		cfg.RelayURL = boot.RelayURL
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = st.WorkerID
	}
	if cfg.Token == "" {
		cfg.Token = st.WorkerToken
	}
	if cfg.WorkspaceRoot == "" && boot.WorkspaceRoot != "" {
		cfg.WorkspaceRoot = boot.WorkspaceRoot
	}
	if len(cfg.Labels) == 0 && len(boot.Labels) > 0 {
		cfg.Labels = boot.Labels
	}

	if cfg.AgentConfigPath == "" {
		if b, err := os.ReadFile(agentCfgPath); err == nil && len(bytes.TrimSpace(b)) > 0 {
			cfg.AgentConfigPath = agentCfgPath
		}
	}
	cfg.EnrollToken = ""
	cfg.StateDir = boot.StateDir
	return &cfg, nil
}

func httpBaseURLFromRelayURL(relayURL string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported relay_url scheme: %s", u.Scheme)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func readBootstrapState(path string) (bootstrapState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return bootstrapState{}, err
	}
	var st bootstrapState
	if err := json.Unmarshal(b, &st); err != nil {
		return bootstrapState{}, err
	}
	return st, nil
}

func writeBootstrapState(path string, st bootstrapState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
