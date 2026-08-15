package worker //nolint:revive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type BootstrapConfig struct { //nolint:revive
	RelayURL          string
	EnrollToken       string
	WorkspaceRoot     string
	HostWorkspaceRoot string
	HostStateRoot     string
	StateDir          string
	LogRoot           string
	Labels            []string
	WorkerID          string
}

type bootstrapState struct {
	WorkerID    string `json:"worker_id"`
	WorkerToken string `json:"worker_token"`
}

type enrollRequest struct {
	EnrollToken string   `json:"enroll_token"`
	WorkerID    string   `json:"worker_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

type enrollResponse struct {
	WorkerID    string `json:"worker_id"`
	WorkerToken string `json:"worker_token"`
}

// BootstrapFromRemote enrolls or pairs with the gateway and returns a Config
// populated with the worker ID, token, and bootstrap parameters. Configuration
// is managed locally — no config is fetched from the gateway.
func BootstrapFromRemote(ctx context.Context, boot BootstrapConfig) (*Config, error) { //nolint:revive
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
	fmt.Printf("Bootstrap relay_url=%s base_url=%s\n", boot.RelayURL, baseURL)
	fmt.Printf("Bootstrap proxy settings: HTTP_PROXY=%t HTTPS_PROXY=%t NO_PROXY=%q\n",
		strings.TrimSpace(os.Getenv("HTTP_PROXY")) != "",
		strings.TrimSpace(os.Getenv("HTTPS_PROXY")) != "",
		strings.TrimSpace(os.Getenv("NO_PROXY")),
	)

	statePath := filepath.Join(stateDir, "state.json")

	st, _ := readBootstrapState(statePath)
	if st.WorkerID == "" {
		st.WorkerID = boot.WorkerID
	}

	client := newBootstrapHTTPClient(baseURL)

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
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return httpStatusError(resp, "enroll failed")
		}
		var out enrollResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		st.WorkerID = out.WorkerID
		st.WorkerToken = out.WorkerToken
		return writeBootstrapState(statePath, st)
	}

	pair := func() error {
		hostname, _ := os.Hostname()
		body, _ := json.Marshal(map[string]any{
			"worker_name": hostname,
			"host_info":   fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
			"labels":      boot.Labels,
		})

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/worker/pairing", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		fmt.Printf("Pairing create url: %s\n", req.URL.String())
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close() //nolint:errcheck

		if resp.StatusCode != http.StatusOK {
			return httpStatusError(resp, "pairing request failed")
		}

		var startResp struct {
			ID        string `json:"id"`
			UserCode  string `json:"user_code"`
			Secret    string `json:"secret"`
			ExpiresAt int64  `json:"expires_at_unix"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&startResp); err != nil {
			return err
		}

		consoleURL := ""
		if u, err := url.Parse(baseURL); err == nil {
			u.Path = "/console"
			u.RawQuery = "pairing=" + startResp.UserCode
			consoleURL = u.String()
		}

		fmt.Printf("\n======================================================\n")
		fmt.Printf("               WORKER PAIRING REQUIRED                \n")
		fmt.Printf("======================================================\n")
		fmt.Printf("\n1. Open Nano Cloud Console:\n")
		if consoleURL != "" {
			fmt.Printf("   %s\n", consoleURL)
		} else {
			fmt.Printf("   %s/console\n", baseURL)
		}
		fmt.Printf("\n2. Approve with this Short Code:\n")
		fmt.Printf("   >>>  %s  <<<\n", startResp.UserCode)
		fmt.Printf("\nWaiting for approval... (Code expires in %d minutes)\n", (startResp.ExpiresAt-time.Now().Unix())/60)
		fmt.Printf("======================================================\n\n")

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/worker/pairing/"+startResp.ID, nil)
				req.Header.Set("Authorization", "Bearer "+startResp.Secret)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Printf(".")
					continue
				}

				if resp.StatusCode == http.StatusUnauthorized {
					resp.Body.Close() //nolint:errcheck
					return errors.New("pairing request unauthorized (secret mismatch?)")
				}
				if resp.StatusCode == http.StatusNotFound {
					resp.Body.Close() //nolint:errcheck
					return errors.New("pairing request not found (rejected or expired?)")
				}
				if resp.StatusCode != http.StatusOK {
					fmt.Printf("\n[pairing] poll status=%d url=%s\n", resp.StatusCode, req.URL.String())
					resp.Body.Close() //nolint:errcheck
					fmt.Printf(".")
					continue
				}

				var statusResp struct {
					Status      string `json:"status"`
					WorkerToken string `json:"worker_token"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&statusResp)
				resp.Body.Close() //nolint:errcheck

				switch statusResp.Status {
				case "approved":
					fmt.Printf("\nWorker approved! Token received.\n")
					st.WorkerToken = statusResp.WorkerToken
					return writeBootstrapState(statePath, st)
				case "rejected":
					return errors.New("pairing request rejected")
				case "expired":
					return errors.New("pairing request expired")
				default:
					fmt.Printf(".")
				}
			}
		}
	}

	// If we already have a token from a previous bootstrap, reuse it.
	// Note: the token is not verified here; if it has been revoked the
	// worker connection will fail and the operator should clear state and
	// re-enroll/pair.
	if st.WorkerToken != "" {
		return buildConfigFromBootstrap(st, boot), nil
	}

	// Need to enroll or pair
	if boot.EnrollToken != "" {
		if err := enroll(); err != nil {
			return nil, err
		}
	} else {
		if err := pair(); err != nil {
			return nil, err
		}
	}

	return buildConfigFromBootstrap(st, boot), nil
}

// buildConfigFromBootstrap constructs a Config from bootstrap parameters and pairing state.
func buildConfigFromBootstrap(st bootstrapState, boot BootstrapConfig) *Config {
	return &Config{
		RelayURL:          boot.RelayURL,
		Token:             st.WorkerToken,
		WorkerID:          st.WorkerID,
		WorkspaceRoot:     boot.WorkspaceRoot,
		HostWorkspaceRoot: boot.HostWorkspaceRoot,
		HostStateRoot:     boot.HostStateRoot,
		StateDir:          boot.StateDir,
		LogRoot:           boot.LogRoot,
		Labels:            boot.Labels,
	}
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
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return "", errors.New("relay_url host is required")
	}
	if strings.TrimSpace(u.Port()) == "" {
		u.Host = net.JoinHostPort(host, "8081")
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

// CheckDockerInfo verifies if Docker daemon is running and accessible.
func CheckDockerInfo(ctx context.Context) {
	fmt.Printf("\n--- Pre-flight Check ---\n")
	fmt.Printf("Checking Docker connection... ")

	cmd := exec.CommandContext(ctx, "docker", "info")
	if err := cmd.Run(); err != nil {
		fmt.Printf("FAILED ❌\n")
		fmt.Printf("Warning: Docker daemon is not running or not accessible.\n")
		fmt.Printf("         Worker may fail to run containers.\n")
		fmt.Printf("         Please start Docker Desktop or check socket permissions.\n")
	} else {
		fmt.Printf("OK ✅\n")
	}
	fmt.Printf("------------------------\n")
}

func newBootstrapHTTPClient(baseURL string) *http.Client {
	targetHost := ""
	if u, err := url.Parse(baseURL); err == nil {
		targetHost = strings.ToLower(strings.TrimSpace(u.Hostname()))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		proxyURL, err := http.ProxyFromEnvironment(req)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		if shouldBypassProxy(req.URL.Hostname(), targetHost) {
			return nil, nil
		}
		return proxyURL, nil
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

func shouldBypassProxy(requestHost string, targetHost string) bool {
	host := strings.ToLower(strings.TrimSpace(requestHost))
	target := strings.ToLower(strings.TrimSpace(targetHost))
	if host == "" {
		return false
	}
	if target != "" && host == target {
		return true
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func httpStatusError(resp *http.Response, prefix string) error {
	method := ""
	reqURL := ""
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		method = resp.Request.Method
		reqURL = resp.Request.URL.String()
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s: %s %s -> status=%d", prefix, method, reqURL, resp.StatusCode)
	}
	return fmt.Errorf("%s: %s %s -> status=%d body=%q", prefix, method, reqURL, resp.StatusCode, message)
}
