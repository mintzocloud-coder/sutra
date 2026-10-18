// SPDX-License-Identifier: AGPL-3.0-or-later
// Commerce rate limiting for the Wave-3 marketplace surface.
//
// The global per-IP limiter (RateLimit) is the outer ceiling for the
// whole API. Commerce endpoints — sellers, products, variants,
// categories, search, cart, orders — get an ADDITIONAL tighter
// per-IP budget: these writes are money-adjacent (checkout, inventory
// adjust, order status transitions, seller onboarding) or
// seller-catalog mutations, and are the abuse surface F6 part 3
// hardens (scraping the catalog, inventory-exhaustion races,
// checkout spam). The two budgets stack: a request needs room in
// BOTH to proceed.
//
// A dedicated error code ("commerce_rate_limited", still a 429 with
// Retry-After) lets clients and dashboards distinguish the commerce
// budget from the global one.
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// commercePathPrefixes is the commerce API surface (F1–F6). Every
// entry matches itself and anything below it ("/v1/products/{id}"),
// on exact prefix boundaries only — "/v1/productivity" does NOT
// match "/v1/products".
var commercePathPrefixes = []string{
	"/v1/sellers",
	"/v1/products",
	"/v1/variants",
	"/v1/categories",
	"/v1/search",
	"/v1/cart",
	"/v1/orders",
}

// IsCommercePath reports whether the request path is part of the
// commerce surface. Exported for tests and for documentation of the
// boundary (see docs/commerce.md).
func IsCommercePath(path string) bool {
	for _, p := range commercePathPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// CommerceRateLimit rejects commerce-surface requests beyond the
// limiter's budget with a 429 "commerce_rate_limited" JSON envelope
// plus Retry-After; everything outside the commerce surface passes
// straight through (and is governed only by the global limiter).
// Set the limiter's rps <= 0 to disable (always allow).
func CommerceRateLimit(l *RateLimiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !IsCommercePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			key := clientIP(r)
			ok, wait := l.Allow(key)
			if l.rps > 0 {
				w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%.0f", l.rps))
			}
			if !ok {
				secs := int(wait.Seconds()) + 1 // ceiling, at least 1s
				w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
				log.Debug("commerce rate limit exceeded",
					"client_ip", key,
					"method", r.Method,
					"path", r.URL.Path,
					"retry_after_s", secs,
					"request_id", FromContext(r.Context()),
				)
				WriteJSONError(w, r, http.StatusTooManyRequests,
					"commerce_rate_limited", "too many commerce requests; slow down and retry")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
