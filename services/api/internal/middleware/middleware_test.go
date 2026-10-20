// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- RequestID ---

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	id := rec.Header().Get(RequestIDHeader)
	if id == "" {
		t.Fatal("response missing X-Request-ID")
	}
	if seen != id {
		t.Fatalf("context request ID %q != response header %q", seen, id)
	}
	// Generated IDs are 128-bit hex (16 random bytes).
	if b, err := hex.DecodeString(id); err != nil || len(b) != 16 {
		t.Fatalf("generated ID %q is not 128-bit hex", id)
	}

	// IDs must be unique per request.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec2.Header().Get(RequestIDHeader) == id {
		t.Fatal("two requests got the same generated request ID")
	}
}

func TestRequestIDReusesInbound(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "caller-correlation-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != "caller-correlation-123" {
		t.Fatalf("inbound ID not echoed: %q", got)
	}
	if seen != "caller-correlation-123" {
		t.Fatalf("context ID = %q, want caller-correlation-123", seen)
	}
}

func TestFromContextEmptyWithoutMiddleware(t *testing.T) {
	if id := FromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); id != "" {
		t.Fatalf("FromContext = %q without middleware, want empty", id)
	}
}

// --- Logger ---

// logRecord replays the JSON access log emitted by one request.
func logRecord(t *testing.T, h http.Handler, req *http.Request) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := Chain(h, RequestID, Logger(log))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	var rec0 map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &rec0); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
	}
	if rec0 == nil {
		t.Fatal("logger emitted nothing")
	}
	return rec0
}

func TestLoggerEmitsStructuredLine(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rec := logRecord(t, inner, httptest.NewRequest(http.MethodPost, "/v1/me", nil))

	if rec["msg"] != "request" {
		t.Fatalf("msg = %v, want request", rec["msg"])
	}
	if rec["method"] != "POST" || rec["path"] != "/v1/me" {
		t.Fatalf("method/path = %v/%v, want POST /v1/me", rec["method"], rec["path"])
	}
	if rec["status"] != float64(http.StatusTeapot) {
		t.Fatalf("status = %v, want 418", rec["status"])
	}
	rid, _ := rec["request_id"].(string)
	if rid == "" {
		t.Fatalf("request_id missing or empty: %v", rec)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Fatalf("duration_ms missing: %v", rec)
	}
	if _, ok := rec["remote"]; !ok {
		t.Fatalf("remote missing: %v", rec)
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
}

func TestLoggerDefaultsTo200(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok") // implicit 200, no WriteHeader call
	})
	rec := logRecord(t, inner, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec["status"] != float64(http.StatusOK) {
		t.Fatalf("status = %v, want 200", rec["status"])
	}
}

// --- Recover ---

func TestRecoverConvertsPanicToEnvelope(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	panicker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	// Production order: RequestID outermost so the 500 carries the ID.
	h := Chain(panicker, RequestID, Recover(log))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic response is not JSON: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok || errObj["code"] != "internal" {
		t.Fatalf("missing internal-code envelope: %q", rec.Body.String())
	}
	rid, _ := errObj["request_id"].(string)
	if rid == "" {
		t.Fatalf("envelope request_id empty: %v", errObj)
	}
	if rec.Header().Get(RequestIDHeader) != rid {
		t.Fatalf("header X-Request-ID %q != envelope request_id %q",
			rec.Header().Get(RequestIDHeader), rid)
	}
	if !strings.Contains(logBuf.String(), "panic recovered") {
		t.Fatalf("panic not logged: %q", logBuf.String())
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("panic value leaked to client: %q", rec.Body.String())
	}
}

func TestRecoverPassesThrough(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("fine"))
	})
	h := Chain(inner, RequestID, Recover(log))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusCreated || rec.Body.String() != "fine" {
		t.Fatalf("non-panic request altered: %d %q", rec.Code, rec.Body.String())
	}
	if logBuf.Len() != 0 {
		t.Fatalf("unexpected log output on clean request: %q", logBuf.String())
	}
}

// --- Chain ---

func TestChainRunsFirstMiddlewareOutermost(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-in")
				next.ServeHTTP(w, r)
				order = append(order, name+"-out")
			})
		}
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	Chain(final, mw("A"), mw("B")).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"A-in", "B-in", "handler", "B-out", "A-out"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// --- MethodNotAllowedHandler ---

// TestMethodNotAllowedJoinsAllow pins the multi-method Allow header to a
// single comma-joined value. A Set+Add sequence emits multiple header
// lines of which Header.Get (and most clients) see only the first — the
// creative-routes test caught Allow="POST" where "POST, GET" was due.
func TestMethodNotAllowedJoinsAllow(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/x", nil)
	MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "POST, GET" {
		t.Errorf("Allow = %q, want %q", got, "POST, GET")
	}
	// Single-method routes keep the plain form.
	rec2 := httptest.NewRecorder()
	MethodNotAllowedHandler(http.MethodPatch)(rec2, req)
	if got := rec2.Header().Get("Allow"); got != "PATCH" {
		t.Errorf("Allow = %q, want %q", got, "PATCH")
	}
}
