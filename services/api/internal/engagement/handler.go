// SPDX-License-Identifier: AGPL-3.0-or-later
package engagement

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// LikeResult is the JSON body for like/unlike responses.
type LikeResult struct {
	Liked     bool  `json:"liked"`
	LikeCount int64 `json:"like_count"`
}

// LikeHandler serves POST /v1/posts/{id}/like and DELETE
// /v1/posts/{id}/like on one route (dispatched by method): like is
// idempotent (201 new, 200 retry — mirroring follow), unlike is
// idempotent 204-safe but returns 200 with the count so clients can
// update UI state without a refetch.
func LikeHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		raw := r.PathValue("id")
		var postID pgtype.UUID
		if err := postID.Scan(raw); err != nil || !postID.Valid {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_post_id", "post id must be a UUID")
			return
		}

		var (
			liked     bool
			likeCount int64
			err       error
		)
		switch r.Method {
		case http.MethodPost:
			var created bool
			created, likeCount, err = svc.Like(r.Context(), postID, callerID)
			if err != nil {
				if errors.Is(err, ErrPostNotFound) {
					middleware.WriteJSONError(w, r, http.StatusNotFound,
						"post_not_found", "post does not exist")
					return
				}
				log.Error("engagement: like failed", "err", err,
					"request_id", middleware.FromContext(r.Context()))
				middleware.WriteStatusError(w, r, http.StatusInternalServerError,
					"internal server error")
				return
			}
			liked = true
			if created {
				w.WriteHeader(http.StatusCreated)
			} else {
				w.WriteHeader(http.StatusOK)
			}
		case http.MethodDelete:
			var removed bool
			removed, likeCount, err = svc.Unlike(r.Context(), postID, callerID)
			if err != nil {
				log.Error("engagement: unlike failed", "err", err,
					"request_id", middleware.FromContext(r.Context()))
				middleware.WriteStatusError(w, r, http.StatusInternalServerError,
					"internal server error")
				return
			}
			liked = !removed // false: the like is now absent
			w.WriteHeader(http.StatusOK)
		default:
			// Unreachable: main.go's methodGuard only admits POST/DELETE.
			middleware.WriteStatusError(w, r, http.StatusMethodNotAllowed,
				"method not allowed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LikeResult{Liked: liked, LikeCount: likeCount})
	}
}
