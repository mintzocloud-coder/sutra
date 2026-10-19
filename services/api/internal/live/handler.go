// SPDX-License-Identifier: AGPL-3.0-or-later
// HTTP handlers for the live seam (G1): room registry + token mint.
//
// All handlers run behind RequireAuth (mounted under /v1/), so the
// subject is always present. The caller's user id is provisioned from
// the subject exactly like the social handlers do — live rooms are
// owned by user ids, not raw subjects.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/realtime"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxLiveBody caps the JSON bodies the live seam accepts: they are
// metadata (title, kind, role) — media never touches the API.
const maxLiveBody = 16 << 10

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// callerUUID provisions the caller's user row from the verified OIDC
// subject (social.CallerUUID's contract, without the social import).
func callerUUID(w http.ResponseWriter, r *http.Request, pool *db.Pool, log *slog.Logger) (pgtype.UUID, bool) {
	ctx := r.Context()
	subject := middleware.SubjectFromContext(ctx)
	u, err := users.Provision(ctx, pool, subject, "")
	if err != nil {
		log.Error("live: provision failed", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil { // cannot happen: id came from our own users table
		log.Error("live: provisioned id not a UUID", "err", err,
			"request_id", middleware.FromContext(ctx))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return pgtype.UUID{}, false
	}
	return id, true
}

// liveError maps package errors to envelope responses; unexpected errors
// are logged and answered 500.
func liveError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	var client bool
	switch {
	case errors.Is(err, ErrLiveDisabled):
		middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
			"live_unavailable", "live rooms are not enabled on this deployment")
		client = true
	case errors.Is(err, ErrNoSuchRoom):
		middleware.WriteJSONError(w, r, http.StatusNotFound,
			"live_room_not_found", "live room not found")
		client = true
	case errors.Is(err, ErrRoomEnded):
		middleware.WriteJSONError(w, r, http.StatusGone,
			"live_room_ended", "this live room has ended")
		client = true
	case errors.Is(err, ErrNotHost):
		middleware.WriteJSONError(w, r, http.StatusForbidden,
			"not_room_host", "only the room host may perform this action")
		client = true
	case errors.Is(err, ErrBadTitle):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_room_title", "title must be 1-120 characters")
		client = true
	case errors.Is(err, ErrBadKind):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_room_kind", "kind must be 'livestream' or 'call'")
		client = true
	case errors.Is(err, ErrBadRole):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_live_role", "role must be 'host' or 'viewer'")
		client = true
	}
	if !client {
		log.Error("live: "+op, "err", err,
			"request_id", middleware.FromContext(r.Context()))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
	}
}

// tokenRequest is POST /v1/live/token.
type tokenRequest struct {
	RoomID string `json:"room_id"`
	Role   string `json:"role"` // host | viewer
	Name   string `json:"name"` // display hint for the SFU participant list
}

type tokenResponse struct {
	Token      string    `json:"token"`
	WSURL      string    `json:"ws_url"`
	RoomID     string    `json:"room_id"`
	RoomName   string    `json:"room_name"` // SFU identifier clients actually join
	Identity   string    `json:"identity"`  // the JWT sub (the caller's subject)
	Role       string    `json:"role"`
	ExpiresAt  time.Time `json:"expires_at"`
	TTLSeconds int64     `json:"ttl_seconds"`
}

// TokenHandler mints a room-scoped LiveKit participant JWT.
//
// POST /v1/live/token {"room_id": "<uuid>", "role": "host"|"viewer", "name": "Display"}
//   - role=host: caller must be the room's host (403 otherwise). The
//     token grants media publish.
//   - role=viewer: any authenticated user. Subscribe + data channel
//     only — no media publish, ever.
//   - the room must be live (410 on ended rooms).
func TokenHandler(svc *Service, pool *db.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerUUID(w, r, pool, log)
		if !ok {
			return
		}
		var req tokenRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxLiveBody)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_request_body", "request body must be JSON with room_id and role")
			return
		}
		var roomID pgtype.UUID
		if err := roomID.Scan(req.RoomID); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_room_id", "room_id must be a UUID")
			return
		}
		room, err := svc.GetRoom(r.Context(), roomID)
		if err != nil {
			liveError(w, r, log, "get room", err)
			return
		}
		if req.Role == RoleHost && !uuidEqual(room.HostID, caller) {
			liveError(w, r, log, "mint host token", ErrNotHost)
			return
		}
		subject := middleware.SubjectFromContext(r.Context())
		token, expiresAt, err := svc.MintToken(room, subject, req.Name, req.Role)
		if err != nil {
			liveError(w, r, log, "mint token", err)
			return
		}
		writeJSON(w, http.StatusOK, tokenResponse{
			Token:      token,
			WSURL:      svc.WSURL(),
			RoomID:     room.ID.String(),
			RoomName:   room.LiveKitRoomName,
			Identity:   subject,
			Role:       req.Role,
			ExpiresAt:  expiresAt.UTC(),
			TTLSeconds: int64(time.Until(expiresAt).Seconds()),
		})
	}
}

// createRoomRequest is POST /v1/live/rooms.
type createRoomRequest struct {
	Title string `json:"title"`
	Kind  string `json:"kind"` // livestream | call; defaults to livestream
}

// RoomsHandler handles POST /v1/live/rooms (open a room as the caller)
// and GET /v1/live/rooms (list what's on air, newest first).
func RoomsHandler(svc *Service, pool *db.Pool, rt realtime.Publisher, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			caller, ok := callerUUID(w, r, pool, log)
			if !ok {
				return
			}
			var req createRoomRequest
			r.Body = http.MaxBytesReader(w, r.Body, maxLiveBody)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_request_body", "request body must be JSON with title")
				return
			}
			room, err := svc.CreateRoom(r.Context(), caller, req.Title, req.Kind)
			if err != nil {
				liveError(w, r, log, "create room", err)
				return
			}
			// Room lifecycle event (G1): live:started fans out on the
			// HOST's user topic, best-effort, in a detached goroutine —
			// a slow chat service can never delay the 201.
			if rt != nil && rt.Enabled() {
				go func() {
					pctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
					defer cancel()
					if err := realtime.PublishLiveStatus(pctx, pool, caller,
						realtime.EventLiveStarted, room.ID.String(), room.LiveKitRoomName,
						room.Title, room.Kind, room.CreatedAt, nil, rt); err != nil {
						log.Warn("realtime: live:started publish failed (best-effort)",
							"room_id", room.ID.String(), "err", err)
					}
				}()
			}
			writeJSON(w, http.StatusCreated, room)
		case http.MethodGet:
			limit := 50
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					limit = n
				}
			}
			rooms, err := svc.ListLiveRooms(r.Context(), limit)
			if err != nil {
				liveError(w, r, log, "list rooms", err)
				return
			}
			if rooms == nil {
				rooms = []*Room{}
			}
			writeJSON(w, http.StatusOK, rooms)
		default:
			middleware.WriteStatusError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// EndRoomHandler handles POST /v1/live/rooms/{id}/end: the host ends
// the room. Publishes live:ended on the host's topic (best-effort) so
// the host's own devices can tear down immediately.
func EndRoomHandler(svc *Service, pool *db.Pool, rt realtime.Publisher, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := callerUUID(w, r, pool, log)
		if !ok {
			return
		}
		var roomID pgtype.UUID
		if err := roomID.Scan(r.PathValue("id")); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_room_id", "room id must be a UUID")
			return
		}
		room, err := svc.EndRoom(r.Context(), roomID, caller)
		if err != nil {
			liveError(w, r, log, "end room", err)
			return
		}
		if rt != nil && rt.Enabled() {
			go func() {
				pctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
				defer cancel()
				if err := realtime.PublishLiveStatus(pctx, pool, caller,
					realtime.EventLiveEnded, room.ID.String(), room.LiveKitRoomName,
					room.Title, room.Kind, room.CreatedAt, room.EndedAt, rt); err != nil {
					log.Warn("realtime: live:ended publish failed (best-effort)",
						"room_id", room.ID.String(), "err", err)
				}
			}()
		}
		writeJSON(w, http.StatusOK, room)
	}
}
