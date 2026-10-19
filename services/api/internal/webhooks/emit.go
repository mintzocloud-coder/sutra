// SPDX-License-Identifier: AGPL-3.0-or-later
package webhooks

// Platform-event fan-out (G2 part 2): the same domain events that go to
// the chat service's realtime topics (order.created, shipment:update,
// live:started, live:ended) are also published to the affected users'
// Svix applications so developers receive them on their registered
// webhook endpoints.
//
// Delivery is deliberately best-effort: a failed publish never fails
// the API request that caused it. The database rows are the source of
// truth; webhook deliveries are a notification hint. Event IDs are
// deterministic per (event, entity) so a crash-retry re-publishes the
// SAME eventID and Svix dedupes instead of double-delivering.

import (
	"context"
	"log/slog"
	"time"
)

// emitTimeout bounds one best-effort platform-event publish (EnsureApp
// + Publish against the Svix server). The caller has already responded;
// this only caps how long a hung Svix server can hold a goroutine.
const emitTimeout = 15 * time.Second

// LivePayload is the developer-webhook payload for live:started /
// live:ended. Timestamps are unix seconds.
type LivePayload struct {
	RoomID     string `json:"room_id"`
	RoomName   string `json:"room_name,omitempty"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	HostUserID string `json:"host_user_id"`
	StartedAt  *int64 `json:"started_at,omitempty"`
	EndedAt    *int64 `json:"ended_at,omitempty"`
}

// EmitBestEffort publishes one platform event to a user's Svix app in a
// detached goroutine with its own deadline. It no-ops on a nil or
// disabled client (webhooks not configured on this deployment), and it
// refuses malformed input loudly in the logs rather than silently
// dropping it. The app is ensured first (GET-first create, idempotent)
// so a publish can never fail just because the app was deleted.
func EmitBestEffort(ctx context.Context, c *Client, userID, eventType string, payload any, eventID string, log *slog.Logger) {
	if c == nil || !c.Enabled() {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if userID == "" || eventType == "" || eventID == "" {
		log.Warn("webhooks: refusing to emit malformed platform event",
			"event", eventType, "has_user", userID != "", "has_event_id", eventID != "")
		return
	}
	go func() {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emitTimeout)
		defer cancel()
		appUID := AppUIDForUser(userID)
		if _, err := c.EnsureApp(ectx, appUID, appUID); err != nil {
			log.Warn("webhooks: platform event dropped (ensure app failed, best-effort)",
				"event", eventType, "app", appUID, "err", err)
			return
		}
		if _, err := c.Publish(ectx, appUID, eventType, payload, eventID); err != nil {
			log.Warn("webhooks: platform event dropped (publish failed, best-effort)",
				"event", eventType, "app", appUID, "err", err)
		}
	}()
}
