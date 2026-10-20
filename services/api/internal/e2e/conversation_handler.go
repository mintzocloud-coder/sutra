// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// roomIDPattern mirrors the chat service's RoomPolicy grammar
// (`^[A-Za-z0-9_-]{1,64}$`) — room ids travel in the channel topic
// "room:<room_id>", and both services must agree on what is well-formed.
var roomIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ConversationE2EHandler serves GET /v1/conversations/{room_id}/e2e —
// the per-conversation E2E flag (I3). The flag is written by the chat
// service when it persists the first valid e2e:session_init in the
// room; this endpoint is read-only.
//
// Responses: 200 {room_id, e2e_enabled, enabled_at}; 400
// invalid_room_id on a malformed id; 404 conversation_not_found on an
// unknown room — which clients must treat as NOT E2E (never claim E2E
// on a conversation where it is not enabled).
func ConversationE2EHandler(svc *Service, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		if !roomIDPattern.MatchString(roomID) {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_room_id", "room id must match [A-Za-z0-9_-]{1,64}")
			return
		}
		st, err := svc.E2EStatus(r.Context(), roomID)
		if err != nil {
			e2eError(w, r, log, "conversation e2e status", err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}
