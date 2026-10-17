// SPDX-License-Identifier: AGPL-3.0-or-later
package users

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// writeError sends a JSON error in the standard envelope shape
// (see A6; kept consistent here so the shape is stable when A6 lands).
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    msg,
			"request_id": middleware.FromContext(r.Context()),
		},
	})
	_, _ = w.Write(body)
}

// MeHandler serves GET /v1/me: the identity record for the caller.
//
// The subject always comes from the verified JWT (RequireAuth runs
// before this handler), and the row is created on first sight — so a
// valid token never gets a 404 here; it gets its own user record.
// Database failures are 500s with the standard envelope, never a stack
// trace or driver error text (those go to the log with the request ID).
func MeHandler(pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		subject := middleware.SubjectFromContext(ctx)
		if subject == "" {
			// Invariant broken: this handler must only be reachable
			// through RequireAuth. Log loudly; do not guess an identity.
			log.Error("me: no subject in context", "request_id", middleware.FromContext(ctx))
			writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
			return
		}

		user, err := Provision(ctx, pool, subject, displayNameFromClaims(middleware.ClaimsFromContext(ctx)))
		if err != nil {
			log.Error("me: provision failed", "err", err, "subject", subject,
				"request_id", middleware.FromContext(ctx))
			writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(user); err != nil {
			// Headers already sent; only the log can record this.
			log.Error("me: encode failed", "err", err,
				"request_id", middleware.FromContext(ctx))
		}
	}
}
