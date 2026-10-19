// SPDX-License-Identifier: AGPL-3.0-or-later
// Crash-host reconciliation (G1 part 2, Wave 4): the webhook covers the
// SFU telling us when rooms finish, but a network partition can eat the
// webhook itself. The reconciler closes that gap: on a schedule it asks
// the SFU (LiveKit RoomService.ListRooms) which rooms actually exist and
// ends registry rows that have been "live" past a grace period without
// an SFU-side room.
//
// The reconciler is FAIL-CLOSED: if the SFU cannot be reached or its
// answer cannot be parsed, it ends NOTHING and reports the error. It
// would rather leave a stale "live" row than mass-end rooms on a fetch
// failure. Every flip still goes through EndRoom (status='live' guard)
// and publishes live:ended, so it composes with the webhook path.
package live

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/realtime"
	"github.com/mintzocloud-coder/sutra/services/api/internal/webhooks"
)

// reconcileListRoomsPath is the LiveKit RoomService twirp endpoint.
// The main signalling port (7880) serves the HTTP API alongside the
// websocket; no separate admin port is needed.
const reconcileListRoomsPath = "/twirp/livekit.RoomService/ListRooms"

// serverVideoGrant is the RoomService-side grant shape. RoomService API
// calls need the roomList grant — never participant grants.
type serverVideoGrant struct {
	RoomList bool `json:"roomList,omitempty"`
	// RoomAdmin is deliberately absent: listing is all we need.
}

type serverTokenClaims struct {
	Issuer    string           `json:"iss"`
	NotBefore int64            `json:"nbf"`
	Expires   int64            `json:"exp"`
	JWTID     string           `json:"jti"`
	Video     serverVideoGrant `json:"video"`
}

// mintServerToken issues a short-lived RoomService token (roomList
// grant only). Stdlib HS256, like the participant mint — no server SDK.
func (s *Service) mintServerToken(ttl time.Duration) (string, error) {
	now := time.Now()
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("live: server token entropy failed: %w", err)
	}
	payload, err := json.Marshal(serverTokenClaims{
		Issuer:    s.apiKey,
		NotBefore: now.Unix() - 5,
		Expires:   now.Add(ttl).Unix(),
		JWTID:     hex.EncodeToString(jti[:]),
		Video:     serverVideoGrant{RoomList: true},
	})
	if err != nil {
		return "", fmt.Errorf("live: marshal server claims: %w", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(s.apiSecret))
	mac.Write([]byte(header + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + body + "." + sig, nil
}

// listRoomsResponse is the subset of LiveKit's ListRoomsResponse we
// need: the SFU-side room names.
type listRoomsResponse struct {
	Rooms []struct {
		Name string `json:"name"`
	} `json:"rooms"`
}

// fetchSFURoomNames asks the SFU which rooms exist right now.
func (s *Service) fetchSFURoomNames(ctx context.Context, apiBaseURL string) (map[string]bool, error) {
	token, err := s.mintServerToken(2 * time.Minute)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(apiBaseURL, "/") + reconcileListRoomsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("live: list-rooms request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("live: list-rooms unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("live: list-rooms read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live: list-rooms status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out listRoomsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("live: list-rooms decode: %w", err)
	}
	names := make(map[string]bool, len(out.Rooms))
	for _, r := range out.Rooms {
		names[r.Name] = true
	}
	return names, nil
}

// staleLiveRooms returns registry rooms still marked live that were
// created before the grace cutoff — the reconciliation candidates.
func (s *Service) staleLiveRooms(ctx context.Context, grace time.Duration) ([]*Room, error) {
	const q = `SELECT id, host_id, title, kind, status, viewer_cap, created_at, ended_at
	           FROM live_rooms
	           WHERE status = 'live' AND created_at < $1
	           ORDER BY created_at ASC`
	rows, err := s.pool.Query(ctx, q, time.Now().Add(-grace))
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

// ReconcileStaleRooms ends registry rooms that are live in our DB but
// absent on the SFU, past the grace period. Returns (ended, checked).
// Fail-closed: any SFU fetch error ends nothing and returns the error.
// Each flip publishes live:ended best-effort (via the detached
// publisher), the same contract as the webhook path.
func (s *Service) ReconcileStaleRooms(ctx context.Context, pool *db.Pool, rt realtime.Publisher, wh *webhooks.Client, apiBaseURL string, grace time.Duration, log *slog.Logger) (ended, checked int, err error) {
	if !s.Enabled() {
		return 0, 0, ErrLiveDisabled
	}
	candidates, err := s.staleLiveRooms(ctx, grace)
	if err != nil {
		return 0, 0, err
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}
	existing, err := s.fetchSFURoomNames(ctx, apiBaseURL)
	if err != nil {
		return 0, 0, err // fail-closed: never end rooms on a fetch failure
	}
	for _, room := range candidates {
		checked++
		if existing[roomNameFor(room.ID)] {
			continue // SFU still has it — genuinely live
		}
		endedRoom, err := s.EndRoom(ctx, room.ID, room.HostID)
		if err != nil {
			if err == pgx.ErrNoRows {
				continue // lost a race with the webhook — fine
			}
			return ended, checked, fmt.Errorf("live: reconcile end %s: %w", room.ID.String(), err)
		}
		publishLiveEnded(endedRoom, pool, rt, wh, log)
		ended++
	}
	return ended, checked, nil
}

// StartReconciler runs the crash-host sweep forever on interval,
// doing one pass immediately at boot. Any pass that fails logs the
// error and waits for the next tick — a down SFU degrades the sweep,
// never the API. Cancel the context to stop it (main wires it to
// process shutdown).
func StartReconciler(ctx context.Context, svc *Service, pool *db.Pool, rt realtime.Publisher, wh *webhooks.Client, apiBaseURL string, interval, grace time.Duration, log *slog.Logger) {
	pass := func() {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		ended, checked, err := svc.ReconcileStaleRooms(cctx, pool, rt, wh, apiBaseURL, grace, log)
		switch {
		case err != nil:
			log.Warn("live: reconcile pass failed (fail-closed, nothing ended)",
				"err", err)
		case checked > 0:
			log.Info("live: reconcile pass complete",
				"checked", checked, "ended", ended)
		}
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
