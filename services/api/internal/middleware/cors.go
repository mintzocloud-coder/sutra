// SPDX-License-Identifier: AGPL-3.0-or-later
// CORS handling for the Sutra API service.
package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig configures the CORS middleware.
type CORSConfig struct {
	// Origins is the allowlist of exact origins, e.g.
	// "https://app.sutra.example". A single "*" entry allows any origin
	// (no credentials are involved: the API authenticates via Bearer
	// tokens, never cookies). Empty disables CORS entirely — no
	// Access-Control-* headers are emitted and preflights fall through
	// to the mux.
	Origins []string
	// MaxAge is the Access-Control-Max-Age for preflight caching, seconds.
	MaxAge int
}

// allowedOrigin reports whether origin may access the API. Returns the
// value to put in Access-Control-Allow-Origin ("*" for wildcard mode).
func (c CORSConfig) allowedOrigin(origin string) (string, bool) {
	for _, o := range c.Origins {
		if o == "*" {
			return "*", true
		}
		if strings.EqualFold(o, origin) {
			return origin, true
		}
	}
	return "", false
}

// CORS implements cross-origin handling for browser clients:
//
//   - Preflight (OPTIONS with Access-Control-Request-Method): if the
//     Origin is allowed, answer 204 directly with the allow-* headers;
//     otherwise let the request fall through to the mux (no reflection
//     of untrusted origins — the classic CORS misconfiguration).
//   - Simple/actual requests: attach Access-Control-Allow-Origin (+ Vary)
//     when the Origin is allowed.
//
// It deliberately does NOT set Access-Control-Allow-Credentials: auth is
// Bearer-based, so browsers never need to send cookies.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	maxAge := strconv.Itoa(cfg.MaxAge)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" || len(cfg.Origins) == 0 {
				next.ServeHTTP(w, r) // not a CORS request, or CORS disabled
				return
			}
			allow, ok := cfg.allowedOrigin(origin)
			if !ok {
				next.ServeHTTP(w, r) // untrusted origin: no CORS headers
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", allow)
			if allow != "*" {
				// Vary on Origin so caches don't poison responses across origins.
				w.Header().Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions &&
				r.Header.Get("Access-Control-Request-Method") != "" {
				// Preflight: answer here, no handler invocation.
				w.Header().Set("Access-Control-Allow-Methods",
					"GET, POST, PUT, PATCH, DELETE, OPTIONS")
				if h := r.Header.Get("Access-Control-Request-Headers"); h != "" {
					w.Header().Set("Access-Control-Allow-Headers", h)
				} else {
					w.Header().Set("Access-Control-Allow-Headers",
						"Authorization, Content-Type, X-Request-ID")
				}
				if maxAge != "0" {
					w.Header().Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
