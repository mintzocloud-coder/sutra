// SPDX-License-Identifier: AGPL-3.0-or-later
package middleware

// Benchmarks for the middleware hot paths (C4).
//
// BenchmarkRateLimiterAllow measures the per-request token-bucket cost
// (map lookup + mutex) that sits in front of every request. Run with a
// saturated-but-not-depleted bucket so the result is the steady-state
// allow-path cost, which is what matters at scale.
//
// BenchmarkErrorEnvelopeWrite measures the JSON-encoding cost shared by
// every non-2xx response the API emits.
//
// Reproduce:  cd services/api && go test -run='^$' -bench=. -benchtime=10s -count=3 ./internal/middleware/

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// requestWithID returns a request carrying a request ID, the way the
// RequestID middleware enriches requests in production. Benchmarks
// against this so envelope bodies carry a realistic request_id field.
func requestWithID() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		req = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	return req
}

func BenchmarkRateLimiterAllow(b *testing.B) {
	// rps so high the bucket never depletes: measures the steady-state
	// allow path (single client key, shared mutex).
	limiter := NewRateLimiter(1e9, 1e9)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, _ := limiter.Allow("10.0.0.1"); !ok {
			b.Fatal("Allow denied with effectively infinite budget")
		}
	}
}

func BenchmarkRateLimiterAllowParallel(b *testing.B) {
	// Many distinct client keys hammering the same limiter from all
	// cores: measures mutex contention in the hot path when the client
	// population is large (the realistic case at scale).
	limiter := NewRateLimiter(1e9, 1e9)
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = "10.0.0." + strconv.Itoa(i)
	}
	var n uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// One atomic per WORKER (not per iteration): each worker gets
		// its own client key, so the measurement is the limiter's
		// mutex + bucket cost, not the key-selection overhead.
		key := keys[int(atomic.AddUint64(&n, 1))%len(keys)]
		for pb.Next() {
			// b.Error (not b.Fatal): FailNow must run on the
			// benchmark's own goroutine; Error is goroutine-safe.
			if ok, _ := limiter.Allow(key); !ok {
				b.Error("Allow denied with effectively infinite budget")
			}
		}
	})
}

func BenchmarkErrorEnvelopeWrite(b *testing.B) {
	req := requestWithID()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		WriteStatusError(rec, req, http.StatusNotFound, "not found")
		if rec.Code != http.StatusNotFound {
			b.Fatalf("status = %d, want 404", rec.Code)
		}
	}
}

func BenchmarkRequestIDOnly(b *testing.B) {
	// The cheapest middleware alone: isolates request-ID generation +
	// header echo cost from the rest of the chain.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := RequestID(inner)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}
}

func BenchmarkSecurityHeadersOnly(b *testing.B) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := SecurityHeaders()(inner)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}
}

// compile-time guard: RequestID enrichment must produce a non-empty ID,
// otherwise the envelope benchmark above is benchmarking the wrong thing.
func TestRequestWithIDHelper(t *testing.T) {
	if id := FromContext(requestWithID().Context()); id == "" {
		t.Fatal("enriched request has empty request ID")
	}
}
