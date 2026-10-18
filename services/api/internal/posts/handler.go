// SPDX-License-Identifier: AGPL-3.0-or-later
package posts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/media"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxPostBody caps POST bodies: posts are small (500 runes + metadata);
// anything bigger is abuse, not a post.
const maxPostBody = 64 << 10

// PublishHooks are side effects the post handlers run after a post is
// durably stored or deleted. The home-feed fan-out (D3b, internal/feed)
// plugs in here; the zero value is a no-op so the posts package stays
// usable standalone (and unit-testable without Valkey).
type PublishHooks struct {
	// AfterCreate fans a new post out to secondary stores (home
	// timelines). A non-nil error triggers the compensating rollback:
	// the post is deleted again and the request fails 503 — 201 always
	// means "visible in home timelines", so a client retry is safe.
	AfterCreate func(ctx context.Context, p *Post) error
	// AfterDelete removes a post from secondary stores. Best-effort:
	// the feed read path drops posts missing from PostgreSQL, so a
	// failure here is logged, never fatal.
	AfterDelete func(ctx context.Context, postID string) error
	// AfterCreateCaptions requests auto-captions for the post's video
	// media (E3) when the client passed auto_caption=true. Best-effort
	// and never rolled back: captioning is async, the post is already
	// durable, and a caption request that never got its answer can be
	// retried. A nil value is a no-op.
	AfterCreateCaptions func(ctx context.Context, p *Post) error
}

// noHooks is the explicit "no secondary stores" PublishHooks for
// call sites that want the zero value spelled out.
var noHooks = PublishHooks{}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// targetPostID parses the {id} path value into a UUID, 400 on garbage —
// without ever touching the database.
func targetPostID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := social.ParseUserID(r.PathValue("id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_post_id", "post id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// clientErr reports whether err is a mapped client error (4xx) rather
// than an unexpected (500) failure.
func clientErr(err error) bool {
	for _, e := range []error{
		social.ErrUserNotFound, ErrPostNotFound, ErrNotAuthor,
		ErrEmptyText, ErrTextTooLong, ErrTooManyMedia, ErrBadMediaID,
		social.ErrInvalidCursor,
		media.ErrUnknownMedia, media.ErrMediaNotReady, media.ErrMediaNotOwned,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	var vel *VelocityLimitedError
	return errors.As(err, &vel)
}

// postError maps package errors to envelope responses; unexpected
// errors are logged by the caller and answered 500.
func postError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	if clientErr(err) {
		var vel *VelocityLimitedError
		switch {
		case errors.Is(err, ErrPostNotFound):
			middleware.WriteJSONError(w, r, http.StatusNotFound,
				"post_not_found", "post not found")
		case errors.Is(err, ErrNotAuthor):
			middleware.WriteJSONError(w, r, http.StatusForbidden,
				"post_not_author", "only the author can delete this post")
		case errors.Is(err, ErrEmptyText):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"empty_post", "post text must not be blank")
		case errors.Is(err, ErrTextTooLong):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"post_too_long", "post text exceeds 500 characters")
		case errors.Is(err, ErrTooManyMedia):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"too_many_media", "a post can reference at most 10 media items")
		case errors.Is(err, ErrBadMediaID):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"invalid_media_id", "media id must match [A-Za-z0-9_-]{1,128}")
		case errors.Is(err, social.ErrInvalidCursor):
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_cursor", "cursor is malformed")
		case errors.As(err, &vel):
			// Tell the client exactly when to retry: the wait until
			// the oldest post in the rolling window ages out. A flat
			// 429 would make clients poll blindly.
			secs := int(vel.RetryAfter / 1e9)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			middleware.WriteJSONError(w, r, http.StatusTooManyRequests,
				"post_velocity_limited", "too many posts: slow down and retry")
		case errors.Is(err, social.ErrUserNotFound):
			middleware.WriteJSONError(w, r, http.StatusNotFound,
				"user_not_found", "user not found")
		case errors.Is(err, media.ErrUnknownMedia):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"unknown_media", "one or more media ids were never uploaded")
		case errors.Is(err, media.ErrMediaNotReady):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"media_not_ready", "all attached media must be ready (upload, then complete, then transcode)")
		case errors.Is(err, media.ErrMediaNotOwned):
			middleware.WriteJSONError(w, r, http.StatusForbidden,
				"media_not_owned", "cannot attach media owned by another user")
		}
		return
	}
	log.Error("posts: "+op+" failed", "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// CreatePostHandler serves POST /v1/posts: publish a post as the caller.
// Unknown JSON fields are rejected (strict API, same rule as profiles).
// After the row is durably stored, hooks.AfterCreate fans it out; if the
// fan-out fails the post is rolled back and the request fails 503, so a
// 201 always means "visible in home timelines" and a retry is safe.
func CreatePostHandler(svc *Service, hooks PublishHooks, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}

		var raw map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPostBody))
		if err := dec.Decode(&raw); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_json", "request body must be a JSON object")
			return
		}
		for key := range raw {
			switch key {
			case "text", "media_ids", "auto_caption":
			default:
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"unknown_field", "unknown field: "+key)
				return
			}
		}

		var text string
		if v, present := raw["text"]; present {
			if err := json.Unmarshal(v, &text); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field text must be a string")
				return
			}
		}
		var mediaIDs []string
		if v, present := raw["media_ids"]; present && string(v) != "null" {
			if err := json.Unmarshal(v, &mediaIDs); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field media_ids must be an array of strings")
				return
			}
		}
		// E3: auto_caption asks the caption worker to transcribe the
		// post's video attachments (VTT/SRT sidecars on the media row).
		// Absent = false; non-bool = 400.
		var autoCaption bool
		if v, present := raw["auto_caption"]; present && string(v) != "null" {
			if err := json.Unmarshal(v, &autoCaption); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field auto_caption must be a boolean")
				return
			}
		}

		post, err := svc.Create(r.Context(), authorID, text, mediaIDs)
		if err != nil {
			postError(w, r, log, "create post", err)
			return
		}
		if hooks.AfterCreate != nil {
			if err := hooks.AfterCreate(r.Context(), post); err != nil {
				// Compensating rollback: the post never became visible,
				// so the client can safely retry the whole request.
				postID, perr := social.ParseUserID(post.ID)
				if perr == nil {
					if rerr := svc.Delete(r.Context(), postID, authorID); rerr != nil {
						log.Error("posts: fan-out failed AND rollback failed — orphan post",
							"post_id", post.ID, "fanout_err", err, "rollback_err", rerr,
							"request_id", middleware.FromContext(r.Context()))
					}
				}
				log.Error("posts: fan-out failed; post rolled back", "post_id", post.ID,
					"err", err, "request_id", middleware.FromContext(r.Context()))
				// 503, not 500: this is an infrastructure outage — the
				// client should back off and retry, not treat it as a bug.
				w.Header().Set("Retry-After", "5")
				middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
					"feed_unavailable", "home feed temporarily unavailable; please retry")
				return
			}
		}
		// E3: caption requests are best-effort and never roll the post
		// back — captioning is async, the post is durable, and
		// RequestCaptions is idempotent, so a client can safely retry.
		if autoCaption && hooks.AfterCreateCaptions != nil {
			if err := hooks.AfterCreateCaptions(r.Context(), post); err != nil {
				log.Error("posts: caption request failed", "post_id", post.ID,
					"err", err, "request_id", middleware.FromContext(r.Context()))
			}
		}
		writeJSON(w, http.StatusCreated, post)
	}
}

// DeletePostHandler serves DELETE /v1/posts/{id}: author-only delete.
// 204 on success; 404 for unknown posts; 403 when the caller is not
// the author. hooks.AfterDelete un-fans the post out of home timelines
// (best-effort — the read path drops missing posts anyway).
func DeletePostHandler(svc *Service, hooks PublishHooks, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetPostID(w, r)
		if !ok {
			return
		}
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		if err := svc.Delete(r.Context(), id, callerID); err != nil {
			postError(w, r, log, "delete post", err)
			return
		}
		if hooks.AfterDelete != nil {
			if err := hooks.AfterDelete(r.Context(), id.String()); err != nil {
				// Best-effort: the feed read path rehydrates pushed ids
				// against PostgreSQL and drops missing rows, so a lost
				// unfan-out leaves a hole, never a ghost post.
				log.Warn("posts: feed unfan-out failed (best-effort; read path self-heals)",
					"post_id", id.String(), "err", err,
					"request_id", middleware.FromContext(r.Context()))
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// UserPostsHandler serves GET /v1/users/{id}/posts: one author's posts,
// newest first, keyset-paginated. 404 for unknown users (the listing
// would otherwise just be empty).
func UserPostsHandler(svc *Service, pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetPostID(w, r)
		if !ok {
			return
		}
		limit, after, err := social.PageParams(r)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_pagination", "limit must be a positive integer and cursor must be valid")
			return
		}
		if _, err := social.GetProfile(r.Context(), pool, id); err != nil {
			postError(w, r, log, "list user posts", err)
			return
		}
		page, err := svc.ListByAuthor(r.Context(), id, limit, after)
		if err != nil {
			postError(w, r, log, "list user posts", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}
