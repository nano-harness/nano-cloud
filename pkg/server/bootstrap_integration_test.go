package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nano-harness/nano-cloud/pkg/worker"
	"github.com/sirupsen/logrus"
)

func TestBootstrapFromRemote_PairingFlow(t *testing.T) {
	storeDir := t.TempDir()
	srv := NewGatewayServerWithLogger(":0", "", storeDir, logrus.New())
	ts := httptest.NewServer(srv.router)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	stateDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run bootstrap in a goroutine because it blocks waiting for approval
	errCh := make(chan error, 1)
	cfgCh := make(chan *worker.Config, 1)

	go func() {
		cfg, err := worker.BootstrapFromRemote(ctx, worker.BootstrapConfig{
			RelayURL:      wsURL,
			StateDir:      stateDir,
			WorkspaceRoot: filepath.Join(stateDir, "workspaces"),
			Labels:        []string{"docker-desktop"},
		})
		if err != nil {
			errCh <- err
			return
		}
		cfgCh <- cfg
	}()

	// Wait a bit for the request to be created
	var pairingID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reqs, err := srv.configStore.ListPairingRequests()
		if err == nil && len(reqs) > 0 {
			pairingID = reqs[0].ID
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if pairingID == "" {
		t.Fatalf("timeout waiting for pairing request creation")
	}

	// Approve it
	if err := srv.configStore.ApprovePairingRequest(pairingID); err != nil {
		t.Fatalf("ApprovePairingRequest: %v", err)
	}

	// Wait for bootstrap to complete
	select {
	case err := <-errCh:
		t.Fatalf("BootstrapFromRemote failed: %v", err)
	case cfg := <-cfgCh:
		if cfg.RelayURL != wsURL {
			t.Fatalf("relay url mismatch: got=%q want=%q", cfg.RelayURL, wsURL)
		}
		if cfg.Token == "" {
			t.Fatalf("expected worker token")
		}
		// Verify resume capability
		cfg2, err := worker.BootstrapFromRemote(ctx, worker.BootstrapConfig{
			RelayURL:      wsURL,
			StateDir:      stateDir,
			WorkspaceRoot: cfg.WorkspaceRoot,
		})
		if err != nil {
			t.Fatalf("BootstrapFromRemote (resume): %v", err)
		}
		if cfg2.WorkerID != cfg.WorkerID {
			t.Fatalf("worker id mismatch after resume: got=%q want=%q", cfg2.WorkerID, cfg.WorkerID)
		}
		if cfg2.Token != cfg.Token {
			t.Fatalf("worker token mismatch after resume")
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for bootstrap completion")
	}
}
