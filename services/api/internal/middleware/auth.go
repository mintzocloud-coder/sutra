// SPDX-License-Identifier: AGPL-3.0-or-later
// Authentication middleware for the Sutra API service.
package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

// subjectKey and claimsKey carry the verified identity in the request
// context. Unexported so handlers must use the accessor functions below.
type subjectKey struct{}
type claimsKey struct{}

// SubjectFromContext returns the verified JWT subject stored by RequireAuth,
// or "" when the request is unauthenticated.
func SubjectFromContext(ctx context.Context) string {
	if sub, ok := ctx.Value(subjectKey{}).(string); ok {
		return sub
	}
	return ""
}

// ClaimsFromContext returns the verified JWT claims stored by RequireAuth,
// or nil when the request is unauthenticated (or bypassed).
func ClaimsFromContext(ctx context.Context) *oidc.Claims {
	if c, ok := ctx.Value(claimsKey{}).(*oidc.Claims); ok {
		return c
	}
	return nil
}

// AuthOptions configures RequireAuth.
type AuthOptions struct {
	// Bypass, when true, skips JWT verification entirely and stamps every
	// request with BypassSubject. FOR LOCAL DEVELOPMENT ONLY — it must
	// never be enabled in production. The service logs a loud warning at
	// startup when this is on.
	Bypass bool
	// BypassSubject is the identity stamped on requests in bypass mode.
	BypassSubject string
}

// RequireAuth verifies the Authorization: Bearer JWT on every request and
// stores the subject and claims in the request context. Requests without a
// token, or with an invalid/expired token, are rejected with 401 JSON.
func RequireAuth(v *oidc.Verifier, log *slog.Logger, opts AuthOptions) func(http.Handler) http.Handler {
	if opts.BypassSubject == "" {
		opts.BypassSubject = "dev-user"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Bypass {
				ctx := context.WithValue(r.Context(), subjectKey{}, opts.BypassSubject)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			claims, err := v.VerifyHeader(r.Context(), r.Header.Get("Authorization"))
			if err != nil {
				// Log at debug: auth failures are routine (expired tokens,
				// missing headers) and must not spam the error log.
				log.Debug("auth rejected", "err", err, "request_id", FromContext(r.Context()))
				WriteStatusError(w, r, http.StatusUnauthorized, "unauthorized")
				return
			}
			ctx := context.WithValue(r.Context(), subjectKey{}, claims.Subject)
			ctx = context.WithValue(ctx, claimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
