package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestConsole_AnonymousPublicView(t *testing.T) {
	// Token is empty -> Anonymous mode
	srv := NewGatewayServerWithLogger(":0", "", t.TempDir(), logrus.New())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	srv.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PUBLIC WORKERS") {
		t.Fatalf("expected public workers section")
	}
	if strings.Contains(body, "PRIVATE WORKERS") {
		t.Fatalf("private section must be hidden without login")
	}
}

func TestConsole_LoginEnablesPrivateView(t *testing.T) {
	// Token is "admin-token" -> Auth enabled
	srv := NewGatewayServerWithLogger(":0", "admin-token", t.TempDir(), logrus.New())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console", nil)
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "LOGIN REQUIRED FOR SENSITIVE DATA") {
		t.Fatalf("expected login panel")
	}
	if strings.Contains(rec.Body.String(), "PRIVATE WORKERS") {
		t.Fatalf("private section must be hidden before login")
	}

	// Login with correct token
	loginForm := url.Values{}
	loginForm.Set("token", "admin-token") // Use token field
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(loginForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie")
	}

	// Access with session cookie
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/console", nil)
	req.AddCookie(cookies[0])
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PRIVATE WORKERS") {
		t.Fatalf("expected private section after login")
	}
	if !strings.Contains(rec.Body.String(), "Create a test run") {
		t.Fatalf("expected console test run form after login")
	}
}

func TestConsole_CreateRunFormReportsUnavailableWorker(t *testing.T) {
	srv := NewGatewayServerWithLogger(":0", "admin-token", t.TempDir(), logrus.New())

	loginForm := url.Values{}
	loginForm.Set("token", "admin-token")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(loginForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie")
	}

	runForm := url.Values{}
	runForm.Set("runtime", "nano_agent")
	runForm.Set("prompt", "hello")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/console/runs", strings.NewReader(runForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookies[0])
	srv.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create run redirect status=%d body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "run=failed") || !strings.Contains(loc, "No+worker+available") {
		t.Fatalf("unexpected redirect location: %s", loc)
	}
}
