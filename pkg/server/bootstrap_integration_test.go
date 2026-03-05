package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nano-harness/nano-cloud/pkg/worker"
	"github.com/sirupsen/logrus"
)

func TestBootstrapFromRemote_WithGatewayStore(t *testing.T) {
	storeDir := t.TempDir()
	toks := enrollTokensFile{Tokens: []enrollTokenRecord{{Token: "enroll-1"}}}
	b, _ := json.Marshal(toks)
	if err := os.WriteFile(filepath.Join(storeDir, "enroll_tokens.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write enroll_tokens.json: %v", err)
	}

	srv := NewGatewayServerWithLogger(":0", "", storeDir, logrus.New())
	ts := httptest.NewServer(srv.router)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	stateDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg1, err := worker.BootstrapFromRemote(ctx, worker.BootstrapConfig{
		RelayURL:      wsURL,
		EnrollToken:   "enroll-1",
		StateDir:      stateDir,
		WorkspaceRoot: filepath.Join(stateDir, "workspaces"),
		Labels:        []string{"docker-desktop"},
	})
	if err != nil {
		t.Fatalf("BootstrapFromRemote: %v", err)
	}
	if cfg1.RelayURL != wsURL {
		t.Fatalf("relay url mismatch: got=%q want=%q", cfg1.RelayURL, wsURL)
	}
	if cfg1.Token == "" {
		t.Fatalf("expected worker token")
	}
	if cfg1.WorkerID == "" {
		t.Fatalf("expected worker id")
	}
	if cfg1.WorkspaceRoot == "" {
		t.Fatalf("expected workspace root")
	}

	cfg2, err := worker.BootstrapFromRemote(ctx, worker.BootstrapConfig{
		RelayURL:      wsURL,
		StateDir:      stateDir,
		WorkspaceRoot: cfg1.WorkspaceRoot,
	})
	if err != nil {
		t.Fatalf("BootstrapFromRemote (resume): %v", err)
	}
	if cfg2.WorkerID != cfg1.WorkerID {
		t.Fatalf("worker id mismatch after resume: got=%q want=%q", cfg2.WorkerID, cfg1.WorkerID)
	}
	if cfg2.Token != cfg1.Token {
		t.Fatalf("worker token mismatch after resume")
	}
}
