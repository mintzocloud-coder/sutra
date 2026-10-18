// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// HomeFeedHandler serves GET /v1/feed/home: the caller's merged home
// timeline (fan-out-on-write + pull-on-read), newest first,
// keyset-paged with an opaque `cursor` query parameter.
//
//   - `limit`: 1..100, default 20 (clamped, like every other listing).
//   - `cursor`: the opaque `next_cursor` from the previous page; empty
//     starts from the newest. Malformed cursors are 400, never a
//     store error.
//   - `translate`: ISO 639-1 target language (hi/gu/ta/bn/te/… — the
//     full set lives in SupportedTranslateTargets). When set, each
//     item's body is translated best-effort (see translate.go);
//     items carry `translated_text` / `translated_from` only when a
//     translation actually happened. An unsupported value is 400 — a
//     client bug, not a feed failure.
//   - A downed timeline store is 503 `feed_unavailable` with Retry-After
//     (infrastructure outage, not a client bug); author timelines and
//     post CRUD keep working — they are pure PostgreSQL.
func HomeFeedHandler(pool *db.Pool, svc *Service, translator *Translator, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}

		limit := DefaultPageLimit
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_pagination", "limit must be a positive integer")
				return
			}
			if n > MaxPageLimit {
				n = MaxPageLimit
			}
			limit = n
		}

		var cursor *FeedCursor
		if s := r.URL.Query().Get("cursor"); s != "" {
			c, err := DecodeFeedCursor(s)
			if err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_cursor", "cursor is malformed")
				return
			}
			cursor = c
		}

		target := r.URL.Query().Get("translate")
		if target != "" && !SupportedTranslateTargets[target] {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_translate_target",
				"translate must be a supported ISO 639-1 code (hi, gu, ta, bn, te, kn, ml, mr, pa, or, as, ur, ne, en)")
			return
		}

		page, err := svc.HomeFeed(r.Context(), callerID, limit, cursor)
		if err != nil {
			if errors.Is(err, ErrFeedUnavailable) {
				w.Header().Set("Retry-After", "5")
				middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
					"feed_unavailable", "home feed temporarily unavailable; please retry")
				return
			}
			log.Error("feed: home feed failed", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusInternalServerError,
				"internal server error")
			return
		}

		if target != "" {
			// Best-effort decoration: translation failures never touch
			// the 200. The subject comes from RequireAuth — it feeds the
			// AI service's per-user translate rate limit.
			subject := middleware.SubjectFromContext(r.Context())
			page.Items = translator.decorateItems(r.Context(), subject, target, page.Items)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(page)
	}
}
