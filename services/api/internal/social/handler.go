// SPDX-License-Identifier: AGPL-3.0-or-later
package social

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxPatchBody caps PATCH bodies: profiles are small; anything bigger is
// abuse, not a profile.
const maxPatchBody = 64 << 10

// FollowHooks are side effects the follow handler runs after a NEW follow
// edge is created. Idempotent retries of an existing edge (200) do NOT
// fire hooks — the user already got their notification the first time.
type FollowHooks struct {
	// AfterFollow runs after a new follow edge is committed. Best-effort:
	// a failure is logged by the hook owner, never returned to the client.
	AfterFollow func(ctx context.Context, follower, followee pgtype.UUID)
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// CallerUUID provisions the caller's user row (first sight) and returns
// its UUID, or writes the error response and returns false. Every /v1/*
// handler below runs behind RequireAuth, so a subject is always present;
// the only failure mode left is the database.
func CallerUUID(w http.ResponseWriter, r *http.Request, pool *db.Pool, log *slog.Logger) (pgtype.UUID, bool) {
	ctx := r.Context()
	subject := middleware.SubjectFromContext(ctx)
	u, err := users.Provision(ctx, pool, subject, "")
	if err != nil {
		log.Error("social: provision failed", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	id, err := ParseUserID(u.ID)
	if err != nil { // cannot happen: the id came from our own users table
		log.Error("social: provisioned id not a UUID", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	return id, true
}

// targetUUID parses the {id} path value into a UUID, 400 on garbage —
// without ever touching the database.
func targetUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := ParseUserID(r.PathValue("id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_user_id", "user id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// clientErr reports whether err is a mapped client error (4xx) rather
// than an unexpected (500) failure.
func clientErr(err error) bool {
	for _, e := range []error{
		ErrUserNotFound, ErrInvalidHandle, ErrHandleTaken, ErrSelfFollow,
		ErrInvalidCursor, ErrFieldTooLong, ErrBadAvatarURL,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	// FollowVelocityLimitedError is a distinct typed error, not a
	// sentinel — matched with errors.As like posts.VelocityLimitedError.
	var fvel *FollowVelocityLimitedError
	return errors.As(err, &fvel)
}

// socialError maps package errors to envelope responses; unexpected
// errors are logged by the caller and answered 500.
func socialError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	if clientErr(err) {
		var fvel *FollowVelocityLimitedError
		switch {
		case errors.Is(err, ErrUserNotFound):
			middleware.WriteJSONError(w, r, http.StatusNotFound,
				"user_not_found", "user not found")
		case errors.Is(err, ErrInvalidHandle):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"invalid_handle", "handle must be 3-20 chars: lowercase letters, digits, underscore")
		case errors.Is(err, ErrHandleTaken):
			middleware.WriteJSONError(w, r, http.StatusConflict,
				"handle_taken", "handle is already taken")
		case errors.Is(err, ErrSelfFollow):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"cannot_follow_self", "you cannot follow yourself")
		case errors.Is(err, ErrInvalidCursor):
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_cursor", "cursor is malformed")
		case errors.Is(err, ErrFieldTooLong):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"field_too_long", "a profile field exceeds its maximum length")
		case errors.Is(err, ErrBadAvatarURL):
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"invalid_avatar_url", "avatar_url must be an http(s) URL")
		case errors.As(err, &fvel):
			// Tell the client exactly when to retry: the wait until
			// the oldest follow in the rolling window ages out. A flat
			// 429 would make clients poll blindly.
			secs := int(fvel.RetryAfter / 1e9)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			middleware.WriteJSONError(w, r, http.StatusTooManyRequests,
				"follow_velocity_limited", "too many new follows: slow down and retry")
		}
		return
	}
	log.Error("social: "+op+" failed", "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// ProfileHandler serves PATCH /v1/me/profile: partial update of the
// caller's public profile, creating the row on first use. Unknown JSON
// fields are rejected (strict API: typos fail loudly instead of being
// silently ignored). Present-but-null clears a field.
func ProfileHandler(pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := CallerUUID(w, r, pool, log)
		if !ok {
			return
		}

		var raw map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPatchBody))
		if err := dec.Decode(&raw); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_json", "request body must be a JSON object")
			return
		}
		// Strict API: unknown keys fail loudly instead of being
		// silently ignored (DisallowUnknownFields only works for
		// structs, so the allowlist is checked explicitly here).
		for key := range raw {
			switch key {
			case "handle", "display_name", "bio", "avatar_url":
			default:
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"unknown_field", "unknown field: "+key)
				return
			}
		}

		var patch ProfilePatch
		for key, val := range raw {
			var s *string
			if string(val) != "null" {
				var str string
				if err := json.Unmarshal(val, &str); err != nil {
					middleware.WriteJSONError(w, r, http.StatusBadRequest,
						"invalid_json", "field "+key+" must be a string or null")
					return
				}
				s = &str
			}
			switch key {
			case "handle":
				patch.Handle, patch.HandleSet = s, true
			case "display_name":
				patch.DisplayName, patch.DisplayNameSet = s, true
			case "bio":
				patch.Bio, patch.BioSet = s, true
			case "avatar_url":
				patch.AvatarURL, patch.AvatarURLSet = s, true
			}
		}

		profile, err := PatchProfile(r.Context(), pool, userID, patch)
		if err != nil {
			socialError(w, r, log, "patch profile", err)
			return
		}
		writeJSON(w, http.StatusOK, profile)
	}
}

// UserHandler serves GET /v1/users/{id}: the public profile. The OIDC
// subject is deliberately NOT exposed — the handle is the public
// identifier.
func UserHandler(pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetUUID(w, r)
		if !ok {
			return
		}
		profile, err := GetProfile(r.Context(), pool, id)
		if err != nil {
			socialError(w, r, log, "get profile", err)
			return
		}
		writeJSON(w, http.StatusOK, profile)
	}
}

// FollowHandler serves POST and DELETE /v1/users/{id}/follow.
// POST is idempotent: 201 on a new edge, 200 when it already existed.
// DELETE is idempotent: 204 whether or not the edge existed.
// POST is guarded by a rolling per-user velocity cap (followCapPerHour,
// <=0 selects DefaultFollowVelocityCap): over the cap is 429
// follow_velocity_limited with a Retry-After header.
func FollowHandler(pool *db.Pool, followCapPerHour int, hooks FollowHooks, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodDelete:
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodDelete)(w, r)
			return
		}
		follower, ok := CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		followee, ok := targetUUID(w, r)
		if !ok {
			return
		}

		if r.Method == http.MethodPost {
			isNew, err := FollowWithCap(r.Context(), pool, follower, followee, followCapPerHour)
			if err != nil {
				socialError(w, r, log, "follow", err)
				return
			}
			status := http.StatusOK
			if isNew {
				status = http.StatusCreated
			}
			writeJSON(w, status, map[string]any{
				"following": true,
				"user_id":   followee.String(),
			})
			if isNew && hooks.AfterFollow != nil {
				hooks.AfterFollow(r.Context(), follower, followee)
			}
			return
		}

		if err := Unfollow(r.Context(), pool, follower, followee); err != nil {
			socialError(w, r, log, "unfollow", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// PageParams parses the limit/cursor query params for the listing
// handlers. A non-positive/non-integer limit or a malformed cursor is a
// 400 (client bugs fail loudly); a limit above the cap clamps instead of
// erroring, since huge pages are a cost issue, not a client bug.
func PageParams(r *http.Request) (int, *Cursor, error) {
	limit := DefaultPageLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return 0, nil, ErrInvalidCursor // surfaced as invalid_pagination below
		}
		if n > MaxPageLimit {
			n = MaxPageLimit
		}
		limit = n
	}
	var after *Cursor
	if s := r.URL.Query().Get("cursor"); s != "" {
		c, err := DecodeCursor(s)
		if err != nil {
			return 0, nil, err
		}
		after = &c
	}
	return limit, after, nil
}

// FollowersHandler serves GET /v1/users/{id}/followers: who follows
// this user, newest first, keyset-paginated.
func FollowersHandler(pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetUUID(w, r)
		if !ok {
			return
		}
		limit, after, err := PageParams(r)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_pagination", "limit must be a positive integer and cursor must be valid")
			return
		}
		// 404 for unknown users (the listing itself would just be empty).
		if _, err := GetProfile(r.Context(), pool, id); err != nil {
			socialError(w, r, log, "list followers", err)
			return
		}
		page, err := ListFollowers(r.Context(), pool, id, limit, after)
		if err != nil {
			socialError(w, r, log, "list followers", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// FollowingHandler serves GET /v1/users/{id}/following: who this user
// follows, newest first, keyset-paginated.
func FollowingHandler(pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetUUID(w, r)
		if !ok {
			return
		}
		limit, after, err := PageParams(r)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_pagination", "limit must be a positive integer and cursor must be valid")
			return
		}
		if _, err := GetProfile(r.Context(), pool, id); err != nil {
			socialError(w, r, log, "list following", err)
			return
		}
		page, err := ListFollowing(r.Context(), pool, id, limit, after)
		if err != nil {
			socialError(w, r, log, "list following", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}
