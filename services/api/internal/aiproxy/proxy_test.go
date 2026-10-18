// SPDX-License-Identifier: AGPL-3.0-or-later
package aiproxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// authed wraps h in RequireAuth bypass mode so the request carries a
// verified subject exactly like production traffic does.
func authed(h http.Handler, subject string) http.Handler {
	return middleware.RequireAuth(nil, testLogger(), middleware.AuthOptions{
		Bypass:        true,
		BypassSubject: subject,
	})(h)
}

func TestNewProxyFailClosed(t *testing.T) {
	if _, err := NewProxy("http://[::1", "tok", testLogger()); err == nil {
		t.Fatal("expected error for unparseable URL with token set")
	}
	if _, err := NewProxy("", "tok", testLogger()); err == nil {
		t.Fatal("expected error for empty URL with token set")
	}
	if _, err := NewProxy("not-a-url", "tok", testLogger()); err == nil {
		t.Fatal("expected error for URL without scheme/host with token set")
	}
	p, err := NewProxy("http://ai:8000", "tok", testLogger())
	if err != nil || !p.Enabled() {
		t.Fatalf("expected enabled proxy, got err=%v", err)
	}
	p, err = NewProxy("http://ai:8000", "", testLogger())
	if err != nil || p.Enabled() {
		t.Fatalf("expected disabled proxy on empty token, got err=%v", err)
	}
}

func TestDisabledHandler503(t *testing.T) {
	p, err := NewProxy("http://ai:8000", "", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ai/healthz", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the JSON envelope: %v", err)
	}
	if body.Error.Code != "service_unavailable" {
		t.Fatalf("want envelope code service_unavailable, got %q", body.Error.Code)
	}
}

func TestProxyForwardsAuthAndSubject(t *testing.T) {
	var gotAuth, gotSubject, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSubject = r.Header.Get("X-Sutra-Subject")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	p, err := NewProxy(upstream.URL, "secret-token", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ai/healthz", nil)
	rec := httptest.NewRecorder()
	authed(p.Handler(), "user-123").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/healthz" {
		t.Fatalf("prefix not stripped: upstream saw path %q", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("upstream did not get the service token, got %q", gotAuth)
	}
	if gotSubject != "user-123" {
		t.Fatalf("upstream did not get the subject header, got %q", gotSubject)
	}
}

func TestProxyBarePrefixMapsToRoot(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, err := NewProxy(upstream.URL, "tok", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ai", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if gotPath != "/" {
		t.Fatalf("bare /v1/ai should map to /, got %q", gotPath)
	}
}

func TestProxyUpstreamDown503(t *testing.T) {
	// Port 1 is never listening: forces the transport error path.
	p, err := NewProxy("http://127.0.0.1:1", "tok", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/ai/healthz", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("want Retry-After on AI outage")
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the JSON envelope: %v", err)
	}
	if body.Error.Code != "service_unavailable" {
		t.Fatalf("want envelope code service_unavailable, got %q", body.Error.Code)
	}
}
