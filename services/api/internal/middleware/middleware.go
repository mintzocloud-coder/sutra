// SPDX-License-Identifier: AGPL-3.0-or-later
// Package middleware provides HTTP middleware for the Sutra API service:
// request IDs, structured access logging, and panic recovery.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// requestIDKey is the context key carrying the request ID.
type requestIDKey struct{}

// RequestIDHeader is the header used to propagate request IDs.
const RequestIDHeader = "X-Request-ID"

// RequestID ensures every request carries an ID: it reuses the inbound
// X-Request-ID when present (so callers can correlate), otherwise generates
// a random 128-bit hex ID. The ID is echoed on the response and stored in
// the request context for handlers and loggers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				// rand.Read only fails if the OS CSPRNG is broken; fall
				// back to a timestamp-based ID rather than failing the request.
				id = time.Now().UTC().Format("20060102150405.000000000")
			} else {
				id = hex.EncodeToString(b[:])
			}
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// FromContext returns the request ID stored by the RequestID middleware,
// or "" when absent.
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// statusRecorder captures the status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Logger emits one structured JSON log line per request: method, path,
// status, duration, request ID, and remote address. It is deliberately
// minimal — no request/response bodies, which may carry PII.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", FromContext(r.Context()),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// Recover catches panics in downstream handlers and converts them into a
// JSON 500 response instead of crashing the process. The panic value and a
// stack trace go to the error log; the client only sees the request ID.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					id := FromContext(r.Context())
					log.Error("panic recovered",
						"panic", rec,
						"request_id", id,
						"method", r.Method,
						"path", r.URL.Path,
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal server error","request_id":"` + id + `"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Chain applies middlewares in order: Chain(mux, A, B) runs A outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
