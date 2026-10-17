// SPDX-License-Identifier: AGPL-3.0-or-later
// Baseline security headers for the Sutra API service.
//
// The API serves JSON only — never HTML — so the policy is strict: nothing
// here should ever be framed, sniffed, or executed. These headers are cheap
// defense-in-depth against XSS-via-content-sniffing, clickjacking of any
// future web surface, and referrer leakage. They must be applied OUTSIDE
// (before) the rate limiter and error paths so even 429/5xx responses
// carry them.
package middleware

import "net/http"

// SecurityHeaders stamps every response with a conservative header set:
//   - X-Content-Type-Options: nosniff — stop MIME sniffing (XSS vector
//     when an attacker tricks a browser into treating JSON as HTML).
//   - X-Frame-Options: DENY — the API has no legit framing use case.
//   - Referrer-Policy: strict-origin-when-cross-origin — never leak full
//     URLs (which may carry IDs/tokens in query strings) to third parties.
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'
//     — belt-and-suspenders for a JSON API: no resource may load, and the
//     (nonexistent) document may not be embedded anywhere.
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, r)
		})
	}
}
