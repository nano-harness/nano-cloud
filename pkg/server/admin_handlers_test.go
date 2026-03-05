package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestAdminAPI_EnrollTokensLifecycle(t *testing.T) {
	dir := t.TempDir()
	srv := NewGatewayServerWithLogger(":0", "admin-token", dir, logrus.New())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/enroll-tokens", nil)
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/enroll-tokens", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"ttl_seconds": 3600})
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/enroll-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Token == "" {
		t.Fatalf("expected token")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/enroll-tokens/"+created.Token+"/revoke", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminAPI_WorkerConfigUpdate(t *testing.T) {
	dir := t.TempDir()
	srv := NewGatewayServerWithLogger(":0", "admin-token", dir, logrus.New())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/enroll-tokens", bytes.NewReader([]byte(`{"ttl_seconds":3600}`)))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create enroll status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Token == "" {
		t.Fatalf("missing enroll token")
	}

	enrollBody, _ := json.Marshal(map[string]any{"enroll_token": created.Token, "labels": []string{"docker-desktop"}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/worker/enroll", bytes.NewReader(enrollBody))
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("worker enroll status=%d body=%s", rec.Code, rec.Body.String())
	}
	var enrollResp struct {
		WorkerID string `json:"worker_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &enrollResp)
	if enrollResp.WorkerID == "" {
		t.Fatalf("missing worker_id")
	}

	newWorkerCfg := "relay_url: ws://localhost:80\nworker_id: " + enrollResp.WorkerID + "\nname: managed-worker\nversion: \"2.0\"\nlabels:\n  - docker-desktop\nworkspace_root: /tmp/nano-workspaces\nenv_passthrough:\n  - NANO_API_KEY\nruntimes:\n  nano_agent:\n    image: nano-agent-runtime:local\n"
	updateBody, _ := json.Marshal(map[string]any{"worker_config_yaml": newWorkerCfg})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/workers/"+enrollResp.WorkerID+"/config", bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
}
