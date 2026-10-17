// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	h := CORS(CORSConfig{Origins: []string{"https://app.example"}, MaxAge: 600})(okHandler())
	req := httptest.NewRequest(http.MethodOptions, "/v1/me", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO not echoed, got %q", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatal("Vary: Origin missing")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow-Methods missing GET, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, X-Request-ID" {
		t.Fatalf("Allow-Headers not echoed, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Fatalf("Max-Age wrong, got %q", got)
	}
}

func TestCORSPreflightUntrustedOrigin(t *testing.T) {
	h := CORS(CORSConfig{Origins: []string{"https://app.example"}})(okHandler())
	req := httptest.NewRequest(http.MethodOptions, "/v1/me", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Falls through to the handler: no CORS headers reflected.
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("ACAO must not be set for untrusted origin")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want fall-through 200, got %d", rec.Code)
	}
}

func TestCORSActualRequestAllowedOrigin(t *testing.T) {
	h := CORS(CORSConfig{Origins: []string{"https://app.example"}})(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("ACAO not set on actual request, got %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

func TestCORSWildcardOrigin(t *testing.T) {
	h := CORS(CORSConfig{Origins: []string{"*"}})(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("want ACAO=*, got %q", got)
	}
	// Wildcard responses are cache-safe without Vary.
	if strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Fatal("wildcard mode should not emit Vary: Origin")
	}
}

func TestCORSDisabled(t *testing.T) {
	h := CORS(CORSConfig{})(okHandler()) // no origins configured
	req := httptest.NewRequest(http.MethodOptions, "/v1/me", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers when disabled")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight should fall through to handler when disabled, got %d", rec.Code)
	}
}
