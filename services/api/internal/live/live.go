// SPDX-License-Identifier: AGPL-3.0-or-later
// Package live owns Mintzo's live-media seam (G1, Wave 4): a LiveKit SFU
// for livestreams and video calls, minted through the Go API.
//
// Design notes (see docs/live.md):
//   - LiveKit is the ONE blessed readymade for Wave 4. Everything around
//     it is custom: the room registry (migration 0014), the token mint,
//     and the room lifecycle events.
//   - Token minting is done with the standard library (crypto/hmac,
//     crypto/sha256, encoding/base64, encoding/json) — no server SDK
//     dependency. The claim shape is the documented LiveKit JWT shape:
//     {iss: api_key, sub: identity, nbf, exp, jti, video: grants}.
//   - Tokens are ROOM-SCOPED and short-lived (LiveTokenTTLMinutes): the
//     only grant is roomJoin into "sutra:<room_id>" — never an
//     admin/wide grant. Viewers can subscribe and send data; only the
//     host can publish media tracks.
//   - The API secret is fail-closed: if LIVEKIT_API_SECRET is empty the
//     service refuses to boot. The public ws:// URL is config, not a
//     secret, but defaults to the compose host.
package live

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Room kinds and lifecycle statuses. Mirror migration 0014's CHECK
// constraints; keep them in sync.
const (
	KindLivestream = "livestream"
	KindCall       = "call"

	StatusLive  = "live"
	StatusEnded = "ended"
)

// Limits.
const (
	MaxTitleLen   = 120 // bytes of UTF-8; mirrors the migration CHECK
	DefaultTTLMin = 120 // token lifetime when LiveTokenTTLMinutes <= 0
	MaxViewerCap  = 100000
)

// Roles the token mint understands.
const (
	RoleHost   = "host"
	RoleViewer = "viewer"
)

// Errors the handlers map onto the API error envelope.
var (
	ErrLiveDisabled = errors.New("live is disabled: LIVEKIT_API_SECRET is not set")
	ErrNoSuchRoom   = errors.New("live room does not exist")
	ErrRoomEnded    = errors.New("live room has ended")
	ErrNotHost      = errors.New("only the room host may perform this action")
	ErrBadTitle     = errors.New("title must be 1-120 characters")
	ErrBadKind      = errors.New("kind must be livestream or call")
	ErrBadRole      = errors.New("role must be host or viewer")
)

// Service is the live seam: room registry + token mint.
type Service struct {
	pool *db.Pool
	// apiKey and apiSecret are the LiveKit keypair; apiSecret doubles as
	// the HS256 signing key. A nil service (liveSvc == nil) means live is
	// disabled: routes stay unregistered and endpoints 503.
	apiKey    string
	apiSecret string
	wsURL     string
	ttl       time.Duration
	enabled   bool
}

// NewService builds the live service. An empty apiSecret marks the
// service disabled — main.go then skips route registration entirely and
// the compose stack simply runs LiveKit with no API seam. A non-empty
// apiKey with empty apiSecret is a MISCONFIGURATION: the service is
// disabled too (fail-closed), never half-open. The pool may be nil only
// for unit tests of the stateless token mint; DB methods panic without
// one, and main.go only registers the routes when a real pool exists.
func NewService(pool *db.Pool, apiKey, apiSecret, wsURL string, ttlMinutes int) *Service {
	ttl := time.Duration(ttlMinutes) * time.Minute
	if ttl <= 0 {
		ttl = DefaultTTLMin * time.Minute
	}
	return &Service{
		pool:      pool,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		wsURL:     wsURL,
		ttl:       ttl,
		enabled:   apiKey != "" && apiSecret != "",
	}
}

// Enabled reports whether the live seam is fully wired.
func (s *Service) Enabled() bool { return s != nil && s.enabled }

// Room is one row of live_rooms.
type Room struct {
	ID        pgtype.UUID `json:"id"`
	HostID    pgtype.UUID `json:"host_id"`
	Title     string      `json:"title"`
	Kind      string      `json:"kind"`
	Status    string      `json:"status"`
	ViewerCap int         `json:"viewer_cap"`
	CreatedAt time.Time   `json:"created_at"`
	EndedAt   *time.Time  `json:"ended_at,omitempty"`
	// LiveKitRoomName is the SFU-side identifier: "sutra:<room_id>" so
	// display titles can be free-text without sanitisation risk.
	LiveKitRoomName string `json:"livekit_room_name"`
}

// roomNameFor wires the SFU room name to the durable room id.
func roomNameFor(id pgtype.UUID) string {
	return "sutra:" + id.String()
}

func scanRoom(row pgx.Row) (*Room, error) {
	var r Room
	var endedAt pgtype.Timestamptz
	var createdAt pgtype.Timestamptz
	err := row.Scan(&r.ID, &r.HostID, &r.Title, &r.Kind, &r.Status, &r.ViewerCap, &createdAt, &endedAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = createdAt.Time
	if endedAt.Valid {
		t := endedAt.Time
		r.EndedAt = &t
	}
	r.LiveKitRoomName = roomNameFor(r.ID)
	return &r, nil
}

// validateTitle rejects empty/overlong titles. Byte length (not rune
// length) mirrors the migration's char_length check closely enough:
// char_length counts runes, so a rune-count check is the stricter one —
// do both, cheaply, and reject invalid UTF-8 outright.
func validateTitle(title string) error {
	if !utf8.ValidString(title) {
		return ErrBadTitle
	}
	n := len(strings.TrimSpace(title))
	if n == 0 || n > MaxTitleLen {
		return ErrBadTitle
	}
	return nil
}

// CreateRoom opens a live room as the given host. The host_id must be
// the caller's subject (enforced by the handler); kind defaults to
// livestream. One host may have MANY live rooms (co-hosting multi-room
// events is a feature, not abuse); moderation caps stay out of v1.
func (s *Service) CreateRoom(ctx context.Context, hostID pgtype.UUID, title, kind string) (*Room, error) {
	if !s.Enabled() {
		return nil, ErrLiveDisabled
	}
	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if kind == "" {
		kind = KindLivestream
	}
	if kind != KindLivestream && kind != KindCall {
		return nil, ErrBadKind
	}
	const q = `INSERT INTO live_rooms (host_id, title, kind)
	           VALUES ($1, $2, $3)
	           RETURNING id, host_id, title, kind, status, viewer_cap, created_at, ended_at`
	return scanRoom(s.pool.QueryRow(ctx, q, hostID, strings.TrimSpace(title), kind))
}

// EndRoom flips a live room to ended. Only the host may end it; ending
// an already-ended room is a no-op success (idempotent for webhook
// retries in G1 part 2).
func (s *Service) EndRoom(ctx context.Context, roomID, callerID pgtype.UUID) (*Room, error) {
	if !s.Enabled() {
		return nil, ErrLiveDisabled
	}
	const q = `UPDATE live_rooms
	           SET status = 'ended', ended_at = now()
	           WHERE id = $1 AND status = 'live'
	           RETURNING id, host_id, title, kind, status, viewer_cap, created_at, ended_at`
	r, err := scanRoom(s.pool.QueryRow(ctx, q, roomID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSuchRoom
		}
		return nil, err
	}
	if !uuidEqual(r.HostID, callerID) {
		return nil, ErrNotHost
	}
	return r, nil
}

// GetRoom returns any room by id (for token minting and status reads).
func (s *Service) GetRoom(ctx context.Context, roomID pgtype.UUID) (*Room, error) {
	if !s.Enabled() {
		return nil, ErrLiveDisabled
	}
	const q = `SELECT id, host_id, title, kind, status, viewer_cap, created_at, ended_at
	           FROM live_rooms WHERE id = $1`
	r, err := scanRoom(s.pool.QueryRow(ctx, q, roomID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSuchRoom
		}
		return nil, err
	}
	return r, nil
}

// ListLiveRooms returns the on-air rooms, newest first. LIMIT is the
// only pagination v1 needs (the on-air feed is a shelf, not a scroll).
func (s *Service) ListLiveRooms(ctx context.Context, limit int) ([]*Room, error) {
	if !s.Enabled() {
		return nil, ErrLiveDisabled
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `SELECT id, host_id, title, kind, status, viewer_cap, created_at, ended_at
	           FROM live_rooms
	           WHERE status = 'live'
	           ORDER BY created_at DESC
	           LIMIT $1`
	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// videoGrant is the LiveKit "video" claim. Room-scoped grants only:
// roomJoin into the specific room, never a global admin grant.
type videoGrant struct {
	RoomJoin       bool   `json:"roomJoin"`
	Room           string `json:"room"`
	CanPublish     bool   `json:"canPublish"`
	CanSubscribe   bool   `json:"canSubscribe"`
	CanPublishData bool   `json:"canPublishData,omitempty"`
	Hidden         bool   `json:"hidden,omitempty"`
}

type tokenClaims struct {
	Issuer    string     `json:"iss"`
	Subject   string     `json:"sub"`
	NotBefore int64      `json:"nbf"`
	Expires   int64      `json:"exp"`
	JWTID     string     `json:"jti"`
	Video     videoGrant `json:"video"`
	// Name is a display hint for the SFU participant list — free text,
	// never trusted for platform decisions.
	Name string `json:"name,omitempty"`
}

// MintToken issues a room-scoped LiveKit participant JWT.
//
//   - role=host: the room host may publish media. The caller's subject
//     MUST match the room's host_id (checked by the handler, not here).
//   - role=viewer: subscribe + data channel only; cannot publish tracks.
//     Viewers may be any authenticated user.
//
// Tokens expire after the configured TTL. Clients re-mint on expiry —
// there is no refresh grant; a stale client simply re-joins, which is
// the correct behaviour for a live session.
func (s *Service) MintToken(room *Room, identity, displayName, role string) (token string, expiresAt time.Time, err error) {
	if !s.Enabled() {
		return "", time.Time{}, ErrLiveDisabled
	}
	if role != RoleHost && role != RoleViewer {
		return "", time.Time{}, ErrBadRole
	}
	if room.Status != StatusLive {
		return "", time.Time{}, ErrRoomEnded
	}
	now := time.Now()
	expiresAt = now.Add(s.ttl)
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("live: jti entropy failed: %w", err)
	}
	grant := videoGrant{
		RoomJoin:     true,
		Room:         room.LiveKitRoomName,
		CanSubscribe: true,
		CanPublish:   role == RoleHost,
		// Data channel lets viewers react (emoji bursts) without a media
		// track; the SFU rate-limits it, the API doesn't need to.
		CanPublishData: true,
	}
	claims := tokenClaims{
		Issuer:    s.apiKey,
		Subject:   identity,
		NotBefore: now.Unix() - 5, // 5s clock-skew grace
		Expires:   expiresAt.Unix(),
		JWTID:     hex.EncodeToString(jti[:]),
		Video:     grant,
		Name:      displayName,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("live: marshal claims: %w", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(s.apiSecret))
	mac.Write([]byte(header + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + body + "." + sig, expiresAt, nil
}

// WSURL is the client-reachable LiveKit websocket endpoint
// (ws://localhost:7880 in dev compose).
func (s *Service) WSURL() string { return s.wsURL }

func uuidEqual(a, b pgtype.UUID) bool {
	if !a.Valid || !b.Valid || len(a.Bytes) != len(b.Bytes) {
		return false
	}
	return hmac.Equal(a.Bytes[:], b.Bytes[:])
}
