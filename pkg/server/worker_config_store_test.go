package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerConfigStore_ConsumeEnrollToken_OneTime(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWorkerConfigStore(dir)
	if err != nil {
		t.Fatalf("NewWorkerConfigStore: %v", err)
	}

	toks := enrollTokensFile{Tokens: []enrollTokenRecord{{Token: "enroll-1"}}}
	b, _ := json.Marshal(toks)
	if err := os.WriteFile(filepath.Join(dir, "enroll_tokens.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write enroll_tokens.json: %v", err)
	}

	workerID, workerToken, version, err := store.ConsumeEnrollToken("enroll-1", "", []string{"docker-desktop"})
	if err != nil {
		t.Fatalf("ConsumeEnrollToken: %v", err)
	}
	if workerID == "" {
		t.Fatalf("expected workerID")
	}
	if workerToken == "" {
		t.Fatalf("expected workerToken")
	}
	if version == "" {
		t.Fatalf("expected config version")
	}

	_, _, _, err = store.ConsumeEnrollToken("enroll-1", "", nil)
	if err == nil {
		t.Fatalf("expected second consume to fail")
	}

	rec, err := store.GetConfigByWorkerToken(workerToken)
	if err != nil {
		t.Fatalf("GetConfigByWorkerToken: %v", err)
	}
	if rec.WorkerID != workerID {
		t.Fatalf("worker id mismatch: got=%q want=%q", rec.WorkerID, workerID)
	}
	if rec.ConfigVersion != version {
		t.Fatalf("version mismatch: got=%q want=%q", rec.ConfigVersion, version)
	}
	if !strings.Contains(rec.WorkerConfigYAML, "worker_id: "+workerID) {
		t.Fatalf("expected worker config yaml to include worker_id")
	}
}
