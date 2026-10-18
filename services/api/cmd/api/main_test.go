// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

// envelope decodes {"error":{...}} from a recorder and returns the inner
// error object.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response lacks error envelope: %q", rec.Body.String())
	}
	return errObj
}

// assertErrorResponse checks status, JSON content type, and the canonical
// envelope fields (code + message + non-empty request_id).
func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	errObj := envelope(t, rec)
	if errObj["code"] != wantCode {
		t.Fatalf("error.code = %v, want %q", errObj["code"], wantCode)
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Fatalf("error.message missing or not a string: %v", errObj)
	}
	rid, ok := errObj["request_id"].(string)
	if !ok || rid == "" {
		t.Fatalf("error.request_id missing or empty: %v", errObj)
	}
	if rec.Header().Get("X-Request-ID") != rid {
		t.Fatalf("response X-Request-ID %q != envelope request_id %q",
			rec.Header().Get("X-Request-ID"), rid)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testVerifier() *oidc.Verifier {
	// Never performs network I/O unless VerifyHeader is called with an
	// actual bearer token (missing/empty headers fail before any fetch).
	return oidc.NewVerifier("http://127.0.0.1:1/jwks.json", "test-issuer", "test-audience")
}

func testConfig(authBypass bool) config.Config {
	return config.Config{
		AuthBypass:     authBypass,
		BypassSubject:  "test-user",
		RateLimitRPS:   1000, // effectively unlimited for tests
		RateLimitBurst: 2000,
		CORSMaxAge:     600,
	}
}

// Pure unit tests: paths that never touch the database run against the
// real production handler with a nil pool. This is safe ONLY because
// /healthz and the outer catch-all never dereference the pool; anything
// DB-backed (readyz, /v1/me) is covered by the gated tests below.

func TestHandler_HealthzAndNotFound_NoDB(t *testing.T) {
	h := newHandler(testConfig(true), nil, testVerifier(), nil, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

// Integration tests against a real PostgreSQL. Run when TEST_DATABASE_URL
// is set; otherwise skip cleanly (CI provides the database).

func testDeps(t *testing.T, authBypass bool) (http.Handler, *db.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping handler integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, testLogger())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return newHandler(testConfig(authBypass), pool, testVerifier(), nil, testLogger()), pool
}

func TestHandler_UnauthorizedEnvelope(t *testing.T) {
	h, _ := testDeps(t, false) // real auth, no bypass
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestHandler_MethodNotAllowedEnvelope(t *testing.T) {
	h, _ := testDeps(t, true) // bypass auth; the method guard answers
	req := httptest.NewRequest(http.MethodPost, "/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorResponse(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func TestHandler_V1UnknownPathEnvelope(t *testing.T) {
	h, _ := testDeps(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

// TestHandler_MeOK exercises the full stack: auth bypass → just-in-time
// provisioning → 200 with the user row. A second call must return the same
// row (idempotent provision over HTTP), and the 200 must carry X-Request-ID
// like every other response.
func TestHandler_MeOK(t *testing.T) {
	h, _ := testDeps(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/me = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("200 response missing X-Request-ID")
	}
	var user map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatalf("me response not JSON: %v", err)
	}
	if user["subject"] != "test-user" {
		t.Fatalf("subject = %v, want test-user", user["subject"])
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET /v1/me = %d (body %q), want 200", rec2.Code, rec2.Body.String())
	}
	var user2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &user2); err != nil {
		t.Fatalf("second me response not JSON: %v", err)
	}
	if user2["id"] != user["id"] {
		t.Fatalf("second call id = %v, want %v (provision not idempotent)", user2["id"], user["id"])
	}
}

func TestHandler_ReadyzOK(t *testing.T) {
	h, _ := testDeps(t, true)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("readyz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestHandler_ReadyzNotReadyEnvelope: with a closed (unusable) pool the
// readiness check fails — and must still answer in the envelope, with the
// request ID echoed on the response header.
func TestHandler_ReadyzNotReadyEnvelope(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping handler integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, testLogger())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	pool.Close() // simulate a dead database

	h := newHandler(testConfig(true), pool, testVerifier(), nil, testLogger())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorResponse(t, rec, http.StatusServiceUnavailable, "service_unavailable")
}
