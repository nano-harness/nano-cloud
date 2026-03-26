package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestPairingApproveByShortCodeFlow(t *testing.T) {
	t.Parallel()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	// Start pairing
	startBody, _ := json.Marshal(pairingStartRequest{
		WorkerName: "e2e-worker",
		HostInfo:   "darwin/arm64",
		Labels:     []string{"e2e"},
	})
	req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/worker/pairing", strings.NewReader(string(startBody)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start pairing: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start pairing status=%d", resp.StatusCode)
	}
	var startOut pairingStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&startOut); err != nil {
		t.Fatalf("decode start resp: %v", err)
	}
	if startOut.ID == "" || startOut.Secret == "" || startOut.UserCode == "" {
		t.Fatalf("expected id/secret/user_code")
	}
	if len(startOut.UserCode) != 6 {
		t.Fatalf("user_code length=%d", len(startOut.UserCode))
	}

	// Approve by short code (admin)
	approveReq, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/admin/pairing/code/"+startOut.UserCode+"/approve", nil)
	approveReq.Header.Set("Authorization", "Bearer "+token)
	approveResp, err := http.DefaultClient.Do(approveReq)
	if err != nil {
		t.Fatalf("approve by code: %v", err)
	}
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("approve by code status=%d", approveResp.StatusCode)
	}

	// Poll pairing status with secret
	pollReq, _ := http.NewRequest(http.MethodGet, gateway.URL+"/v1/worker/pairing/"+startOut.ID, nil)
	pollReq.Header.Set("Authorization", "Bearer "+startOut.Secret)
	pollResp, err := http.DefaultClient.Do(pollReq)
	if err != nil {
		t.Fatalf("poll pairing: %v", err)
	}
	defer pollResp.Body.Close()
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll pairing status=%d", pollResp.StatusCode)
	}
	var pollOut pairingStatusResponse
	if err := json.NewDecoder(pollResp.Body).Decode(&pollOut); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if pollOut.Status != PairingStatusApproved || strings.TrimSpace(pollOut.WorkerToken) == "" {
		t.Fatalf("expected approved with worker_token")
	}

	// Fetch worker config using issued token (sanity)
	cfgReq, _ := http.NewRequest(http.MethodGet, gateway.URL+"/v1/worker/config", nil)
	cfgReq.Header.Set("Authorization", "Bearer "+pollOut.WorkerToken)
	cfgResp, err := http.DefaultClient.Do(cfgReq)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer cfgResp.Body.Close()
	if cfgResp.StatusCode != http.StatusOK {
		t.Fatalf("get config status=%d", cfgResp.StatusCode)
	}
}

func TestConsoleApproveByCodePrefill(t *testing.T) {
	t.Parallel()

	token := "dev-token"
	srv := NewGatewayServerWithLogger(":0", token, t.TempDir(), logrus.New())
	gateway := httptest.NewServer(srv.router)
	defer gateway.Close()

	// Login to console
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	loginForm := url.Values{}
	loginForm.Set("token", token)
	lreq, _ := http.NewRequest(http.MethodPost, gateway.URL+"/console/login", strings.NewReader(loginForm.Encode()))
	lreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	lresp, err := client.Do(lreq)
	if err != nil {
		t.Fatalf("console login: %v", err)
	}
	_ = lresp.Body.Close()
	if lresp.StatusCode == http.StatusSeeOther {
		// OK, expected redirect
	} else if lresp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", lresp.StatusCode)
	}

	// Access console with prefill code
	code := "ABC123"
	cresp, err := client.Get(gateway.URL + "/console?pairing=" + code)
	if err != nil {
		t.Fatalf("console get: %v", err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("console status=%d", cresp.StatusCode)
	}
	// Read small body and check the code is present as value
	buf := make([]byte, 4096)
	n, _ := cresp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `value="`+code+`"`) {
		t.Fatalf("console page should prefill code")
	}
}
