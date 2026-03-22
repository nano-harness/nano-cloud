package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestWorkerConfigHandlers_ETagNotModified(t *testing.T) {
	dir := t.TempDir()
	srv := NewGatewayServerWithLogger(":0", "", dir, logrus.New())

	// 1. Manually create an approved worker via store to skip HTTP pairing dance
	id, secret, _, err := srv.configStore.CreatePairingRequest("test-worker", "linux/amd64", []string{"docker-desktop"})
	if err != nil {
		t.Fatalf("CreatePairingRequest: %v", err)
	}
	if err := srv.configStore.ApprovePairingRequest(id); err != nil {
		t.Fatalf("ApprovePairingRequest: %v", err)
	}
	_, workerToken, err := srv.configStore.PollPairingRequest(id, secret)
	if err != nil {
		t.Fatalf("PollPairingRequest: %v", err)
	}

	// 2. Get Config first time to get version
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/worker/config", nil)
	req1.Header.Set("Authorization", "Bearer "+workerToken)
	srv.router.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("get config status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	configVersion := rec1.Header().Get("ETag")
	if configVersion == "" {
		t.Fatalf("missing ETag header")
	}

	// 3. Get Config with If-None-Match
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/worker/config", nil)
	req2.Header.Set("Authorization", "Bearer "+workerToken)
	req2.Header.Set("If-None-Match", configVersion)
	srv.router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
