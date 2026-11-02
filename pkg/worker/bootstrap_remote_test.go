package worker

import (
	"testing"
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

// TestBuildConfigFromBootstrap verifies that buildConfigFromBootstrap
// correctly assembles a Config from bootstrap parameters and pairing state.
func TestBuildConfigFromBootstrap(t *testing.T) {
	st := bootstrapState{
		WorkerID:    "test-worker-id",
		WorkerToken: "test-token",
	}
	boot := BootstrapConfig{
		RelayURL:          "ws://gateway.example.com:9999",
		WorkspaceRoot:     "/tmp/workspaces",
		HostWorkspaceRoot: "/host/workspaces",
		HostStateRoot:     "/host/state",
		StateDir:          "/tmp/state",
		LogRoot:           "/tmp/logs",
		Labels:            []string{"gpu", "fast"},
	}

	cfg := buildConfigFromBootstrap(st, boot)
	if cfg.RelayURL != boot.RelayURL {
		t.Fatalf("RelayURL mismatch: got=%q want=%q", cfg.RelayURL, boot.RelayURL)
	}
	if cfg.Token != st.WorkerToken {
		t.Fatalf("Token mismatch: got=%q want=%q", cfg.Token, st.WorkerToken)
	}
	if cfg.WorkerID != st.WorkerID {
		t.Fatalf("WorkerID mismatch: got=%q want=%q", cfg.WorkerID, st.WorkerID)
	}
	if cfg.WorkspaceRoot != boot.WorkspaceRoot {
		t.Fatalf("WorkspaceRoot mismatch: got=%q want=%q", cfg.WorkspaceRoot, boot.WorkspaceRoot)
	}
	if cfg.HostWorkspaceRoot != boot.HostWorkspaceRoot {
		t.Fatalf("HostWorkspaceRoot mismatch: got=%q want=%q", cfg.HostWorkspaceRoot, boot.HostWorkspaceRoot)
	}
	if cfg.LogRoot != boot.LogRoot {
		t.Fatalf("LogRoot mismatch: got=%q want=%q", cfg.LogRoot, boot.LogRoot)
	}
	if len(cfg.Labels) != 2 || cfg.Labels[0] != "gpu" || cfg.Labels[1] != "fast" {
		t.Fatalf("Labels mismatch: got=%v", cfg.Labels)
	}
}
