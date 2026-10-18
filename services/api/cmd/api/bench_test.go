// SPDX-License-Identifier: AGPL-3.0-or-later
package main

// Benchmarks for the Sutra API HTTP layer (C4).
//
// Scope: pure HTTP overhead — mux routing, the full middleware chain
// (RequestID, Recover, SecurityHeaders, CORS, RateLimit, Logger), and
// handler encoding. DB-free and network-free (httptest), rate limiter
// effectively unlimited, so `go test -bench` runs anywhere.
//
// What the numbers mean (and don't): they characterize per-request
// framework overhead, NOT end-to-end service latency. Real requests add
// PostgreSQL round-trips (/v1/me), JWKS fetches on cache miss, TLS, and
// network time. See docs/benchmarks.md for the full methodology and the
// recorded baselines.
//
// Reproduce:  cd services/api && go test -run='^$' -bench=. -benchtime=10s -count=3 ./...

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/config"
)

// benchConfig mirrors testConfig but with the rate limiter fully disabled
// (RateLimitRPS <= 0 means "always allow" in internal/middleware). The
// benchmarks measure the HTTP stack, not the limiter: testConfig's
// 1000 rps / 2000 burst is "effectively unlimited" for sequential tests,
// but b.RunParallel bursts straight through it and the run 429s itself
// into a FAIL (seen in CI and reproduced locally).
func benchConfig() config.Config {
	c := testConfig(false)
	c.RateLimitRPS = 0
	return c
}

// benchHandler builds the exact production wiring (newHandler) with the
// rate limiter disabled so benchmarks measure the stack,
// not the limiter. pool/feedSvc/mediaSvc are nil: none of these
// benchmarks touch /readyz, /v1/feed/* or /v1/media/*, the routes that
// dereference them.
func benchHandler(b *testing.B) http.Handler {
	b.Helper()
	return newHandler(benchConfig(), nil, testVerifier(), nil, nil, nil, nil, testLogger(), nil)
}

// serveOnce runs one request through h; shared by the sequential
// benchmarks. A fresh recorder per iteration isolates response writes.
func serveOnce(b *testing.B, h http.Handler, method, target string, headers map[string]string) {
	b.Helper()
	b.ReportAllocs()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			b.Fatalf("unexpected status %d (body %q)", rec.Code, rec.Body.String())
		}
	}
}

// BenchmarkHealthz: GET /healthz through the full chain. This is the
// cheapest possible request (no auth, no DB): the floor of API latency.
func BenchmarkHealthz(b *testing.B) {
	h := benchHandler(b)
	serveOnce(b, h, http.MethodGet, "/healthz", nil)
}

// BenchmarkHealthzParallel: same as above across all cores. Shows how
// the middleware stack scales; a healthy stack scales ~linearly with
// GOMAXPROCS (no global lock in the hot path).
func BenchmarkHealthzParallel(b *testing.B) {
	h := benchHandler(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				// b.Error (not b.Fatal): FailNow must run on the
				// benchmark's own goroutine; Error is goroutine-safe.
				b.Errorf("unexpected status %d", rec.Code)
			}
		}
	})
}

// BenchmarkNotFound404: unknown path → 404 JSON envelope. Covers mux
// miss + the standard error-encoding path every client-visible error
// shares.
func BenchmarkNotFound404(b *testing.B) {
	h := benchHandler(b)
	serveOnce(b, h, http.MethodGet, "/no/such/path", nil)
}

// BenchmarkUnauthorized401: GET /v1/me without a token → 401 envelope.
// Exercises the RequireAuth fast-reject path (missing header; no JWKS
// fetch, no DB).
func BenchmarkUnauthorized401(b *testing.B) {
	h := benchHandler(b)
	serveOnce(b, h, http.MethodGet, "/v1/me", nil)
}

// BenchmarkMethodNotAllowed405: POST /v1/me → 405 envelope + Allow
// header. Covers the explicit method-guard path.
func BenchmarkMethodNotAllowed405(b *testing.B) {
	h := benchHandler(b)
	serveOnce(b, h, http.MethodPost, "/v1/me", nil)
}
