package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (w *Worker) remoteConfigLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	lastAckedVersion := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cfg := w.getConfig()
			if cfg == nil || cfg.StateDir == "" || cfg.RelayURL == "" {
				continue
			}

			baseURL, err := httpBaseURLFromRelayURL(cfg.RelayURL)
			if err != nil {
				continue
			}
			client := newBootstrapHTTPClient(baseURL)

			statePath := filepath.Join(cfg.StateDir, "state.json")
			workerCfgPath := filepath.Join(cfg.StateDir, "worker-config.yaml")
			agentCfgPath := filepath.Join(cfg.StateDir, "agent-config.yaml")

			st, err := readBootstrapState(statePath)
			if err != nil {
				st = bootstrapState{WorkerID: cfg.WorkerID, WorkerToken: cfg.Token}
			}
			if st.WorkerToken == "" {
				continue
			}

			if lastAckedVersion == "" && st.ConfigVersion != "" {
				if w.ackAppliedConfig(ctx, client, baseURL, st.WorkerToken, st.ConfigVersion) == nil {
					lastAckedVersion = st.ConfigVersion
				}
			}

			cfgResp, notModified, status, err := w.fetchRemoteConfig(ctx, client, baseURL, st)
			if err != nil {
				continue
			}
			if status == http.StatusNotModified && notModified {
				continue
			}
			if status != http.StatusOK || cfgResp == nil {
				continue
			}

			if err := os.WriteFile(workerCfgPath, []byte(cfgResp.WorkerConfigYAML), 0o600); err != nil {
				continue
			}
			if cfgResp.AgentConfigYAML != "" {
				_ = os.WriteFile(agentCfgPath, []byte(cfgResp.AgentConfigYAML), 0o600)
			}

			st.ConfigVersion = cfgResp.ConfigVersion
			_ = writeBootstrapState(statePath, st)

			merged, err := parseAndMergeWorkerConfig(cfgResp.WorkerConfigYAML, st, BootstrapConfig{
				RelayURL:          cfg.RelayURL,
				WorkspaceRoot:     cfg.WorkspaceRoot,
				HostWorkspaceRoot: cfg.HostWorkspaceRoot,
				HostStateRoot:     cfg.HostStateRoot,
				StateDir:          cfg.StateDir,
				Labels:            cfg.Labels,
				WorkerID:          cfg.WorkerID,
			}, agentCfgPath)
			if err != nil {
				continue
			}
			w.setConfig(merged)

			if st.ConfigVersion != "" && st.ConfigVersion != lastAckedVersion {
				if w.ackAppliedConfig(ctx, client, baseURL, st.WorkerToken, st.ConfigVersion) == nil {
					lastAckedVersion = st.ConfigVersion
				}
			}
		}
	}
}

func (w *Worker) fetchRemoteConfig(ctx context.Context, client *http.Client, baseURL string, st bootstrapState) (*getConfigResponse, bool, int, error) {
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
	defer resp.Body.Close() //nolint:errcheck

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

func (w *Worker) ackAppliedConfig(ctx context.Context, client *http.Client, baseURL string, workerToken string, configVersion string) error {
	body, _ := json.Marshal(map[string]string{"config_version": configVersion})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/worker/config/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if workerToken != "" {
		req.Header.Set("Authorization", "Bearer "+workerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ack config failed: status=%d", resp.StatusCode)
	}
	return nil
}
