package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestWorkerConfigHandlers_ETagNotModified(t *testing.T) {
	dir := t.TempDir()
	toks := enrollTokensFile{Tokens: []enrollTokenRecord{{Token: "enroll-1"}}}
	b, _ := json.Marshal(toks)
	if err := os.WriteFile(filepath.Join(dir, "enroll_tokens.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write enroll_tokens.json: %v", err)
	}

	srv := NewGatewayServerWithLogger(":0", "", dir, logrus.New())

	enrollReqBody, _ := json.Marshal(map[string]any{
		"enroll_token": "enroll-1",
		"labels":       []string{"docker-desktop"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/worker/enroll", bytes.NewReader(enrollReqBody))
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll status=%d body=%s", rec.Code, rec.Body.String())
	}
	var enrollResp struct {
		WorkerToken   string `json:"worker_token"`
		ConfigVersion string `json:"config_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enrollResp); err != nil {
		t.Fatalf("decode enroll resp: %v", err)
	}
	if enrollResp.WorkerToken == "" || enrollResp.ConfigVersion == "" {
		t.Fatalf("missing worker_token/config_version")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/worker/config", nil)
	req2.Header.Set("Authorization", "Bearer "+enrollResp.WorkerToken)
	req2.Header.Set("If-None-Match", `"`+enrollResp.ConfigVersion+`"`)
	srv.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
