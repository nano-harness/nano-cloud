package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestAdminAPI_WorkerConfigUpdate(t *testing.T) {
	dir := t.TempDir()
	srv := NewGatewayServerWithLogger(":0", "admin-token", dir, logrus.New())

	// 1. Create a worker via pairing flow (internal simulation)
	id, secret, _, err := srv.configStore.CreatePairingRequest("test-worker", "linux/amd64", []string{"docker-desktop"})
	if err != nil {
		t.Fatalf("CreatePairingRequest: %v", err)
	}
	if err := srv.configStore.ApprovePairingRequest(id); err != nil {
		t.Fatalf("ApprovePairingRequest: %v", err)
	}
	_, _, err = srv.configStore.PollPairingRequest(id, secret)
	if err != nil {
		t.Fatalf("PollPairingRequest: %v", err)
	}
	// We need the worker ID. The token hash index maps to worker ID.
	// But PollPairingRequest returns token, not ID.
	// We can list workers to find it, or inspect internal state.
	// Let's list workers.
	workers, err := srv.configStore.ListWorkers()
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(workers))
	}
	workerID := workers[0].WorkerID

	// 2. Update config
	newWorkerCfg := "relay_url: ws://localhost:80\nworker_id: " + workerID + "\nname: managed-worker\nversion: \"2.0\"\nlabels:\n  - docker-desktop\nworkspace_root: /tmp/nano-workspaces\nenv_passthrough:\n  - NANO_API_KEY\nruntimes:\n  nano_agent:\n    image: nano-agent-runtime:local\n"
	updateBody, _ := json.Marshal(map[string]any{"worker_config_yaml": newWorkerCfg})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/workers/"+workerID+"/config", bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 3. Verify update
	workers, _ = srv.configStore.ListWorkers()
	if workers[0].WorkerConfigYAML != newWorkerCfg {
		t.Fatalf("config mismatch")
	}
}
