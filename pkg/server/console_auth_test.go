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
	t.Setenv("CONSOLE_USERNAME", "")
	t.Setenv("CONSOLE_PASSWORD", "")

	srv := NewGatewayServerWithLogger(":0", "admin-token", t.TempDir(), logrus.New())
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
	t.Setenv("CONSOLE_USERNAME", "alice")
	t.Setenv("CONSOLE_PASSWORD", "secret-pass")

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

	loginForm := url.Values{}
	loginForm.Set("username", "alice")
	loginForm.Set("password", "secret-pass")
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
}
