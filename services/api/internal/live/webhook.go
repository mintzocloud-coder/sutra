// SPDX-License-Identifier: AGPL-3.0-or-later
// LiveKit webhook ingress (G1 part 2, Wave 4): the SFU tells the API
// about room lifecycle it owns — room_finished flips the registry row
// to ended and publishes live:ended, so a crashed host's room doesn't
// stay "live" forever.
//
// Authentication follows the documented LiveKit server-webhook scheme:
// LiveKit signs every webhook with the API secret (HS256) and sends it
// as `Authorization: <api_key> <jwt>`. The JWT carries iss=<api_key>,
// nbf/exp, and a `sha256` claim binding the exact request body — the
// handler verifies all four before trusting a single byte. Unsigned,
// mis-signed, expired, or body-tampered posts are rejected 401, never
// acted on. The webhook endpoint is NOT behind the OIDC RequireAuth
// (LiveKit has no user credential); the signature IS the auth.
//
// Reconciliation (crash-host sweeper) lives in reconcile.go.
package live

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/realtime"
	"github.com/mintzocloud-coder/sutra/services/api/internal/webhooks"
)

// Webhook events the ingress handles. room_finished is acted on; every
// other event the SFU can send is acknowledged and ignored — the
// registry is the platform's source of truth, the SFU is a notifier.
const (
	EventRoomFinished    = "room_finished"
	EventRoomStarted     = "room_started"
	EventParticipantLeft = "participant_left"
)

// maxWebhookBody caps webhook posts: they are metadata (event + room
// descriptors) — media never touches the API.
const maxWebhookBody = 64 << 10

// webhookBodyLeeway is how far a webhook token's nbf/exp may skew from
// our clock before we reject it. LiveKit mints webhook tokens with a
// 5-minute validity; 60s skew tolerance is generous without being lax.
const webhookClockLeeway = 60 * time.Second

// ErrWebhookUnauthorized is returned when a webhook post fails
// signature verification. The handler maps it to 401.
var ErrWebhookUnauthorized = errors.New("livekit webhook signature invalid")

// ErrUnknownSFURoom is returned when a webhook names an SFU room that
// is not ours (name doesn't parse as "sutra:<uuid>"). The handler
// acknowledges and ignores these — LiveKit may serve test rooms.
var ErrUnknownSFURoom = errors.New("sfu room name is not a sutra room")

// webhookEvent is the LiveKit server-webhook payload. Only the fields
// the ingress needs are decoded; the rest (egress, ingress info,
// track descriptors) is ignored.
type webhookEvent struct {
	Event       string              `json:"event"`
	ID          string              `json:"id"`
	Room        webhookRoom         `json:"room"`
	Participant *webhookParticipant `json:"participant,omitempty"`
	NumDropped  int                 `json:"num_dropped,omitempty"`
}

type webhookRoom struct {
	SID  string `json:"sid"`
	Name string `json:"name"`
}

type webhookParticipant struct {
	Identity string `json:"identity"`
}

// webhookClaims is the LiveKit webhook-token claim shape (per the
// livekit-server WebhookReceiver): iss = api key, nbf/exp, and the
// sha256 claim binding the exact body bytes.
type webhookClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	NotBefore int64  `json:"nbf"`
	Expires   int64  `json:"exp"`
	SHA256    string `json:"sha256"`
}

// parseSFURoomID extracts the registry room id from an SFU room name.
// Our rooms are always "sutra:<room_id>" (roomNameFor); anything else
// is not ours.
func parseSFURoomID(sfuName string) (pgtype.UUID, error) {
	var id pgtype.UUID
	name, ok := strings.CutPrefix(sfuName, "sutra:")
	if !ok || name == "" {
		return id, fmt.Errorf("%w: %q", ErrUnknownSFURoom, sfuName)
	}
	if err := id.Scan(name); err != nil {
		return id, fmt.Errorf("%w: %q", ErrUnknownSFURoom, sfuName)
	}
	if !id.Valid {
		return id, fmt.Errorf("%w: %q", ErrUnknownSFURoom, sfuName)
	}
	return id, nil
}

// verifyWebhookToken authenticates a LiveKit webhook post. authHeader
// must be "<api_key> <jwt>"; the JWT is HS256-signed with the API
// secret and its sha256 claim must match the exact body bytes.
func verifyWebhookToken(authHeader string, body []byte, apiKey, apiSecret string) error {
	if apiKey == "" || apiSecret == "" {
		return ErrWebhookUnauthorized
	}
	key, token, ok := strings.Cut(strings.TrimSpace(authHeader), " ")
	if !ok || strings.TrimSpace(key) != apiKey || strings.TrimSpace(token) == "" {
		return ErrWebhookUnauthorized
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ErrWebhookUnauthorized
	}
	// Verify the signature BEFORE trusting any claim.
	mac := hmac.New(sha256.New, []byte(apiSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return ErrWebhookUnauthorized
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrWebhookUnauthorized
	}
	var claims webhookClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ErrWebhookUnauthorized
	}
	if claims.Issuer != apiKey {
		return ErrWebhookUnauthorized
	}
	now := time.Now()
	if claims.NotBefore > now.Add(webhookClockLeeway).Unix() ||
		claims.Expires < now.Add(-webhookClockLeeway).Unix() {
		return ErrWebhookUnauthorized
	}
	// Bind the token to the exact body: a signature lifted off one
	// webhook must not replay against another body. livekit-server
	// writes the claim in standard base64; accept the URL variant too
	// so we don't 401 on an encoding-only difference.
	sum := sha256.Sum256(body)
	wantStd := base64.StdEncoding.EncodeToString(sum[:])
	wantURL := base64.RawURLEncoding.EncodeToString(sum[:])
	if !hmac.Equal([]byte(claims.SHA256), []byte(wantStd)) &&
		!hmac.Equal([]byte(claims.SHA256), []byte(wantURL)) {
		return ErrWebhookUnauthorized
	}
	return nil
}

// EndRoomBySFUName flips the registry room named by the SFU (e.g. the
// room_finished webhook) to ended. It is idempotent: an already-ended
// room is a no-op success, so webhook retries are safe. The returned
// bool reports whether the row actually flipped this call.
func (s *Service) EndRoomBySFUName(ctx context.Context, sfuName string) (*Room, bool, error) {
	if !s.Enabled() {
		return nil, false, ErrLiveDisabled
	}
	id, err := parseSFURoomID(sfuName)
	if err != nil {
		return nil, false, err
	}
	room, err := s.GetRoom(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if room.Status != StatusLive {
		return room, false, nil
	}
	// The SFU speaks for the host here: the room is gone on the SFU,
	// so the host's authority is moot — pass the room's own host id.
	ended, err := s.EndRoom(ctx, id, room.HostID)
	if err != nil {
		return nil, false, err
	}
	return ended, true, nil
}

// publishLiveEnded emits live:ended on the host's topic, best-effort,
// in a detached goroutine — the SFU's webhook retry budget must never
// wait on the chat service. The same event also fans out to the host's
// Svix app (G2 part 2) — EmitBestEffort no-ops on a nil/disabled
// client, so this stays safe when webhooks are unconfigured.
func publishLiveEnded(room *Room, pool *db.Pool, rt realtime.Publisher, wh *webhooks.Client, log *slog.Logger) {
	webhooks.EmitBestEffort(context.Background(), wh, room.HostID.String(), realtime.EventLiveEnded,
		livePayload(room), "live-ended:"+room.ID.String(), log)
	if rt == nil || !rt.Enabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := realtime.PublishLiveStatus(ctx, pool, room.HostID,
			realtime.EventLiveEnded, room.ID.String(), room.LiveKitRoomName,
			room.Title, room.Kind, room.CreatedAt, room.EndedAt, rt); err != nil {
			log.Warn("realtime: webhook live:ended publish failed (best-effort)",
				"room_id", room.ID.String(), "err", err)
		}
	}()
}

// WebhookHandler is POST /internal/livekit/webhook — the LiveKit SFU's
// server-webhook ingress. Authenticated by the LiveKit-signed token,
// NOT by OIDC (registered on the root mux, outside RequireAuth).
//
// Behaviour:
//   - room_finished naming one of our rooms: flip the registry to
//     ended (idempotent), publish live:ended, 200.
//   - room_finished naming nothing of ours, or any other event:
//     acknowledge 200, act on nothing.
//   - unsigned / mis-signed / expired / body-tampered posts: 401.
//     Never decode-then-trust: verification runs before the JSON
//     parse affects any state.
func WebhookHandler(svc *Service, pool *db.Pool, rt realtime.Publisher, wh *webhooks.Client, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || !svc.Enabled() {
			middleware.WriteStatusError(w, r, http.StatusServiceUnavailable, "live is not enabled on this deployment")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				body = append(body, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		if err := verifyWebhookToken(r.Header.Get("Authorization"), body, svc.apiKey, svc.apiSecret); err != nil {
			log.Warn("live: webhook auth failed", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteJSONError(w, r, http.StatusUnauthorized,
				"live_webhook_unauthorized", "invalid livekit webhook signature")
			return
		}
		var ev webhookEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_webhook_body", "webhook body must be a LiveKit event object")
			return
		}
		if ev.Event != EventRoomFinished {
			// Acknowledged, ignored: the registry is the platform's
			// source of truth; the SFU only ever reports finishes.
			log.Info("live: webhook event acknowledged (ignored)", "event", ev.Event,
				"room", ev.Room.Name, "request_id", middleware.FromContext(r.Context()))
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "handled": false, "event": ev.Event})
			return
		}
		room, flipped, err := svc.EndRoomBySFUName(r.Context(), ev.Room.Name)
		if err != nil {
			if errors.Is(err, ErrUnknownSFURoom) {
				log.Warn("live: room_finished for non-sutra room (ignored)",
					"room", ev.Room.Name, "request_id", middleware.FromContext(r.Context()))
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "handled": false, "reason": "unknown_room"})
				return
			}
			liveError(w, r, log, "webhook end room", err)
			return
		}
		if flipped {
			publishLiveEnded(room, pool, rt, wh, log)
		}
		log.Info("live: room_finished reconciled", "room_id", room.ID.String(),
			"flipped", flipped, "request_id", middleware.FromContext(r.Context()))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "handled": true, "flipped": flipped, "room_id": room.ID.String()})
	}
}
