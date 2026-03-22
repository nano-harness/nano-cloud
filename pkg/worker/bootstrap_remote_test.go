package worker

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestHTTPBaseURLFromRelayURL(t *testing.T) {
	cases := []struct {
		name     string
		relayURL string
		want     string
	}{
		{
			name:     "keep explicit port",
			relayURL: "ws://localhost:8081/v1/worker/connect",
			want:     "http://localhost:8081",
		},
		{
			name:     "default missing port to 8081 for ws",
			relayURL: "ws://localhost",
			want:     "http://localhost:8081",
		},
		{
			name:     "default missing port to 8081 for wss",
			relayURL: "wss://gateway.example.com/path?x=1#frag",
			want:     "https://gateway.example.com:8081",
		},
		{
			name:     "default missing port to 8081 for http",
			relayURL: "http://127.0.0.1",
			want:     "http://127.0.0.1:8081",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := httpBaseURLFromRelayURL(tc.relayURL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("base url mismatch: got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestHTTPBaseURLFromRelayURLInvalid(t *testing.T) {
	cases := []string{
		"",
		"localhost:8081",
		"ftp://localhost:8081",
		"ws://",
	}

	for _, relayURL := range cases {
		t.Run(relayURL, func(t *testing.T) {
			if _, err := httpBaseURLFromRelayURL(relayURL); err == nil {
				t.Fatalf("expected error for relay_url=%q", relayURL)
			}
		})
	}
}

// TestParseAndMergeWorkerConfigRelayURLPort verifies that the user-configured
// relay URL port (from boot.RelayURL / -relay flag) is preserved when the
// server-side worker config provides a relay_url without an explicit port.
// This is the root cause of the 502 bug: a portless server relay_url used to
// silently override boot.RelayURL, causing dial() to connect on port 80/443
// instead of the intended gateway port.
func TestParseAndMergeWorkerConfigRelayURLPort(t *testing.T) {
	st := bootstrapState{
		WorkerID:    "test-worker",
		WorkerToken: "test-token",
	}

	cases := []struct {
		name         string
		bootRelayURL string
		serverRelay  string // relay_url field in the server-side YAML
		wantRelayURL string
	}{
		{
			name:         "server relay_url empty: use boot.RelayURL",
			bootRelayURL: "ws://gateway.example.com:9999",
			serverRelay:  "",
			wantRelayURL: "ws://gateway.example.com:9999",
		},
		{
			name:         "server relay_url has no port: prefer boot.RelayURL to preserve configured port",
			bootRelayURL: "ws://gateway.example.com:9999",
			serverRelay:  "ws://gateway.example.com",
			wantRelayURL: "ws://gateway.example.com:9999",
		},
		{
			name:         "server relay_url has explicit port: admin override wins",
			bootRelayURL: "ws://gateway.example.com:9999",
			serverRelay:  "ws://gateway.example.com:7777",
			wantRelayURL: "ws://gateway.example.com:7777",
		},
		{
			name:         "server relay_url has different host and port: admin override wins",
			bootRelayURL: "ws://old-gateway.example.com:9999",
			serverRelay:  "ws://new-gateway.example.com:8081",
			wantRelayURL: "ws://new-gateway.example.com:8081",
		},
		{
			name:         "wss server relay_url has no port: prefer boot.RelayURL",
			bootRelayURL: "wss://gateway.example.com:443",
			serverRelay:  "wss://gateway.example.com",
			wantRelayURL: "wss://gateway.example.com:443",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			// agentCfgPath points to a non-existent file inside the temp dir;
			// parseAndMergeWorkerConfig silently skips it when the file is absent.
			agentCfgPath := filepath.Join(stateDir, "agent-config.yaml")

			// Build a minimal worker config YAML with the given relay_url.
			serverCfg := Config{
				RelayURL: tc.serverRelay,
				WorkerID: "test-worker",
				Name:     "test",
				Version:  "1.0",
			}
			yamlBytes, err := yaml.Marshal(&serverCfg)
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}

			boot := BootstrapConfig{
				RelayURL: tc.bootRelayURL,
				StateDir: stateDir,
				WorkerID: "test-worker",
			}

			got, err := parseAndMergeWorkerConfig(string(yamlBytes), st, boot, agentCfgPath)
			if err != nil {
				t.Fatalf("parseAndMergeWorkerConfig: %v", err)
			}
			if got.RelayURL != tc.wantRelayURL {
				t.Fatalf("RelayURL mismatch: got=%q want=%q", got.RelayURL, tc.wantRelayURL)
			}
		})
	}
}
