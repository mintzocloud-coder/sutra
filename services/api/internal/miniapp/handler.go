// SPDX-License-Identifier: AGPL-3.0-or-later
// HTTP handlers for the mini-app registry (G3 part 1).
//
// All routes mount under /v1/ behind RequireAuth, so the OIDC subject is
// always present; callers resolve to their user row exactly like the
// social handlers do (just-in-time provisioning).
//
// Routes:
//
//	POST   /v1/mini-apps                 register (draft) → 201
//	GET    /v1/mini-apps                 public directory (published) → 200
//	GET    /v1/mini-apps/{id}            read (404 on archived) → 200
//	PATCH  /v1/mini-apps/{id}            owner update → 200
//	POST   /v1/mini-apps/{id}/publish    draft/suspended → published → 200
//	POST   /v1/mini-apps/{id}/suspend    published → suspended → 200
//	DELETE /v1/mini-apps/{id}            archive (owner) → 204
//	POST   /v1/mini-apps/{id}/install    install (published only) → 201/200
//	DELETE /v1/mini-apps/{id}/install    uninstall (idempotent) → 204
//	POST   /v1/mini-apps/{id}/launch     launch gate: app + signed token → 200
//	GET    /v1/me/mini-apps              my installs → 200
package miniapp

import (
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

// maxMiniAppBody caps registry JSON bodies: metadata + manifests only,
// media never touches this surface.
const maxMiniAppBody = 64 << 10

// DefaultPageLimit is the directory page size when the client omits it;
// MaxPageLimit clamps explicit requests (cost guard, not a client bug).
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// callerUUID provisions the caller's user row from the verified OIDC
// subject (social.CallerUUID's contract, without the import).
func callerUUID(w http.ResponseWriter, r *http.Request, pool *db.Pool, log *slog.Logger) (pgtype.UUID, bool) {
	ctx := r.Context()
	subject := middleware.SubjectFromContext(ctx)
	u, err := users.Provision(ctx, pool, subject, "")
	if err != nil {
		log.Error("miniapp: provision failed", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil { // cannot happen: id came from our own users table
		log.Error("miniapp: provisioned id not a UUID", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	return id, true
}

// targetUUID parses the {id} path value as a UUID.
func targetUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_app_id", "app id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// clientErr reports whether err is a mapped client error (4xx) rather
// than an unexpected (500) failure.
func clientErr(err error) bool {
	for _, e := range []error{
		ErrMiniAppNotFound, ErrSlugTaken, ErrInvalidSlug, ErrInvalidManifest,
		ErrInvalidField, ErrBadStatusTransition, ErrNotOwner, ErrInstallNotAllowed,
		ErrInvalidCursor, ErrLaunchNotAllowed, ErrLaunchSecretMissing,
		ErrUnknownBridgeMethod,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// miniappError maps package errors to envelope responses; unexpected
// errors are logged and answered 500.
func miniappError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	if clientErr(err) {
		status := http.StatusBadRequest
		code := "bad_request"
		msg := "invalid request"
		switch {
		case errors.Is(err, ErrMiniAppNotFound):
			status, code, msg = http.StatusNotFound, "mini_app_not_found", "mini app not found"
		case errors.Is(err, ErrSlugTaken):
			status, code, msg = http.StatusConflict, "slug_taken", "this slug is already taken"
		case errors.Is(err, ErrInvalidSlug):
			code, msg = "invalid_slug", "slug must match [a-z0-9-]{3,40}"
		case errors.Is(err, ErrInvalidManifest):
			code, msg = "invalid_manifest", err.Error()
		case errors.Is(err, ErrInvalidField):
			code, msg = "invalid_field", err.Error()
		case errors.Is(err, ErrBadStatusTransition):
			code, msg = "invalid_status_transition", "this status change is not allowed"
		case errors.Is(err, ErrNotOwner):
			status, code, msg = http.StatusForbidden, "not_owner", "only the app developer can do this"
		case errors.Is(err, ErrInstallNotAllowed):
			status, code, msg = http.StatusUnprocessableEntity, "install_not_allowed", err.Error()
		case errors.Is(err, ErrInvalidCursor):
			code, msg = "invalid_pagination", "limit must be a positive integer and cursor must be valid"
		case errors.Is(err, ErrLaunchNotAllowed):
			status, code, msg = http.StatusForbidden, "launch_not_allowed", "this mini app cannot be launched by you"
		case errors.Is(err, ErrLaunchSecretMissing):
			status, code, msg = http.StatusServiceUnavailable, "launch_unavailable", "mini-app launches are temporarily unavailable"
		}
		middleware.WriteJSONError(w, r, status, code, msg)
		return
	}
	log.Error("miniapp: "+op, "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// decodeBody reads a bounded JSON body with unknown fields rejected
// (the registry is strict: typos fail loudly, never silently ignored).
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxMiniAppBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_json", "request body must be valid JSON with known fields only")
		return false
	}
	return true
}

// AppsMux serves POST /v1/mini-apps (register) and GET /v1/mini-apps
// (directory) on the collection route.
func AppsMux(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			registerHandler(pool, svc, log).ServeHTTP(w, r)
		case http.MethodGet:
			directoryHandler(svc, log).ServeHTTP(w, r)
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(w, r)
		}
	}
}

// AppMux serves the item routes: GET/PATCH/DELETE
// /v1/mini-apps/{id}, POST /v1/mini-apps/{id}/publish,
// POST /v1/mini-apps/{id}/suspend, POST/DELETE /v1/mini-apps/{id}/install,
// POST /v1/mini-apps/{id}/launch.
//
// launchSecret is the HMAC key for the launch token (MINIAPP_LAUNCH_SECRET);
// empty disables the launch endpoint fail-closed (503).
func AppMux(pool *db.Pool, svc *Service, log *slog.Logger, launchSecret []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetUUID(w, r)
		if !ok {
			return
		}
		action := r.PathValue("action")
		switch {
		case action == "launch" && r.Method == http.MethodPost:
			launchHandler(pool, svc, log, launchSecret).ServeHTTP(w, r)
		case action == "" && r.Method == http.MethodGet:
			app, err := svc.Get(r.Context(), id)
			if err != nil {
				miniappError(w, r, log, "get app", err)
				return
			}
			writeJSON(w, http.StatusOK, app)
		case action == "" && r.Method == http.MethodPatch:
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			var in UpdateInput
			if !decodeBody(w, r, &in) {
				return
			}
			app, err := svc.Update(r.Context(), id, caller, in)
			if err != nil {
				miniappError(w, r, log, "update app", err)
				return
			}
			writeJSON(w, http.StatusOK, app)
		case action == "" && r.Method == http.MethodDelete:
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			if _, err := svc.Transition(r.Context(), id, caller, StatusArchived); err != nil {
				miniappError(w, r, log, "archive app", err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case action == "publish" && r.Method == http.MethodPost:
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			app, err := svc.Transition(r.Context(), id, caller, StatusPublished)
			if err != nil {
				miniappError(w, r, log, "publish app", err)
				return
			}
			writeJSON(w, http.StatusOK, app)
		case action == "suspend" && r.Method == http.MethodPost:
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			app, err := svc.Transition(r.Context(), id, caller, StatusSuspended)
			if err != nil {
				miniappError(w, r, log, "suspend app", err)
				return
			}
			writeJSON(w, http.StatusOK, app)
		case action == "install" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			if r.Method == http.MethodDelete {
				if err := svc.Uninstall(r.Context(), caller, id); err != nil {
					miniappError(w, r, log, "uninstall app", err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			isNew, err := svc.Install(r.Context(), caller, id)
			if err != nil {
				miniappError(w, r, log, "install app", err)
				return
			}
			status := http.StatusOK
			if isNew {
				status = http.StatusCreated
			}
			writeJSON(w, status, map[string]any{"installed": true, "app_id": id.String()})
		default:
			middleware.MethodNotAllowedHandler(http.MethodGet, http.MethodPatch, http.MethodDelete, http.MethodPost)(w, r)
		}
	}
}

// registerHandler serves POST /v1/mini-apps: register a new draft app.
func registerHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerUUID(w, r, pool, log)
		if !ok {
			return
		}
		var in RegisterInput
		if !decodeBody(w, r, &in) {
			return
		}
		app, err := svc.Register(r.Context(), caller, in)
		if err != nil {
			miniappError(w, r, log, "register app", err)
			return
		}
		writeJSON(w, http.StatusCreated, app)
	}
}

// directoryHandler serves GET /v1/mini-apps: the public directory of
// published apps, newest first, keyset-paginated. A malformed cursor or
// limit is 400 (fail loudly); an over-large limit clamps (cost guard).
func directoryHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := DefaultPageLimit
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_pagination", "limit must be a positive integer and cursor must be valid")
				return
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
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_pagination", "limit must be a positive integer and cursor must be valid")
				return
			}
			after = &c
		}
		page, err := svc.Directory(r.Context(), limit, after)
		if err != nil {
			miniappError(w, r, log, "list directory", err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// launchHandler serves POST /v1/mini-apps/{id}/launch: the sandboxed
// frame's entry gate. Returns the registry row (so the client can boot
// the frame) plus a short-lived signed launch token for attribution.
// The body is empty; the launch is a pure gate, not a write.
//
// Rules: the owner may launch in any status (draft preview for
// testing); everyone else needs the install edge (draft is never
// launchable by non-owners). An unset launch secret fails closed with
// 503 launch_unavailable.
func launchHandler(pool *db.Pool, svc *Service, log *slog.Logger, launchSecret []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(launchSecret) == 0 {
			middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
				"launch_unavailable", "mini-app launches are temporarily unavailable")
			return
		}
		id, ok := targetUUID(w, r)
		if !ok {
			return
		}
		caller, ok := callerUUID(w, r, pool, log)
		if !ok {
			return
		}
		launch, err := svc.Launch(r.Context(), caller, id, launchSecret)
		if err != nil {
			miniappError(w, r, log, "launch app", err)
			return
		}
		writeJSON(w, http.StatusOK, launch)
	}
}

// MyInstallsHandler serves GET /v1/me/mini-apps: the caller's installed
// shelf with the rows the client needs to boot each sandboxed frame
// (entry_url + manifest), install timestamp first.
func MyInstallsHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerUUID(w, r, pool, log)
		if !ok {
			return
		}
		apps, err := svc.Installed(r.Context(), caller)
		if err != nil {
			miniappError(w, r, log, "list installs", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
	}
}
