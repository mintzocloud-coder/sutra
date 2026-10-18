// SPDX-License-Identifier: AGPL-3.0-or-later
// Package realtime publishes domain events (new posts, new followers,
// shipment updates) to the chat service, which fans them out on Phoenix
// `user:<subject>` topics.
// It is the Go-side half of the D5 contract — the chat side (the internal
// HTTP endpoint, topic join auth, broadcast) is defined in
// docs/realtime-events.md and lands with the Elixir work.
//
// Delivery is deliberately best-effort: a failed publish never fails the
// API request that caused it. The post/follow writes are the source of
// truth; realtime events are a notification hint. Wave 2 adds a persistent
// inbox with delivery receipts on top of this contract.
package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// Event names the chat service understands. Reserved for Wave 2: post:like,
// post:reply, follow:request — do not reuse these names for anything else.
const (
	EventPostNew     = "post:new"
	EventFollowerNew = "follower:new"
	// EventShipmentUpdate is a Wave-3 commerce event: the API publishes it
	// when a shipment's lifecycle status (or its COD money) changes, and
	// the chat fans it out on the BUYER's user:<subject> topic. F6's saga
	// orchestrator is the producer; this package only owns the wire shape.
	EventShipmentUpdate = "shipment:update"
)

// internalPath is the chat service's internal event endpoint. It is fixed
// so operators cannot accidentally point the publisher at a public route.
const internalPath = "/internal/events"

// MaxTopicsPerCall caps how many recipient topics ride in one HTTP call.
// Past it, Publish chunks into multiple calls: a celebrity-scale burst
// must not build a multi-megabyte JSON body or a single point of failure.
const MaxTopicsPerCall = 500

// publishTimeout bounds one attempt. The caller decides the failure
// policy — the publisher itself never retries: retries belong in Wave 2's
// inbox, not in best-effort hints.
const publishTimeout = 5 * time.Second

// Envelope is the request body POSTed to the chat internal endpoint.
type Envelope struct {
	Topics    []string `json:"topics"`     // e.g. ["user:<oidc-subject>"]
	Event     string   `json:"event"`      // EventPostNew | EventFollowerNew
	Payload   any      `json:"payload"`    // event-specific shape
	DedupeKey string   `json:"dedupe_key"` // idempotency key for the chat side
}

// PostNewPayload is the EventPostNew payload: everything a client needs to
// render a "new post from someone you follow" notification without a
// follow-up fetch.
type PostNewPayload struct {
	PostID       string    `json:"post_id"`
	AuthorID     string    `json:"author_id"`
	AuthorHandle *string   `json:"author_handle,omitempty"`
	Text         string    `json:"text"`
	MediaIDs     []string  `json:"media_ids"`
	CreatedAt    time.Time `json:"created_at"`
}

// FollowerNewPayload is the EventFollowerNew payload.
type FollowerNewPayload struct {
	FollowerID     string    `json:"follower_id"`
	FollowerHandle *string   `json:"follower_handle,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ShipmentUpdatePayload is the EventShipmentUpdate payload: everything a
// buyer-side client needs to render an order-tracking update without a
// follow-up fetch. COD money fields are best-effort mirror state — the
// shipments table (migration 0012) is the source of truth; the event is
// a notification hint, same as post:new / follower:new.
type ShipmentUpdatePayload struct {
	ShipmentID        string    `json:"shipment_id"`
	OrderID           string    `json:"order_id"`
	Status            string    `json:"status"` // one of the 8 TrackingStatus wire values
	TrackingNumber    string    `json:"tracking_number,omitempty"`
	CODEExpectedPaise int64     `json:"cod_expected_paise"`
	CODCollectedPaise *int64    `json:"cod_collected_paise,omitempty"` // nil = not reported yet
	UpdatedAt         time.Time `json:"updated_at"`
}

// Publisher sends event envelopes to the chat service.
type Publisher interface {
	// Publish delivers env to the chat internal endpoint, chunking topics
	// past MaxTopicsPerCall. An error means the event was NOT delivered.
	Publish(ctx context.Context, env Envelope) error
	// Enabled reports whether Publish actually sends anything. The disabled
	// publisher is a no-op — used when no service token is configured.
	Enabled() bool
}

// disabledPublisher is the no-op Publisher for deployments without a
// configured chat service token (dev default). Events are dropped silently
// but loudly logged once at construction, so nobody mistakes silence for
// delivery.
type disabledPublisher struct{}

func (disabledPublisher) Publish(context.Context, Envelope) error { return nil }
func (disabledPublisher) Enabled() bool                           { return false }

// httpPublisher POSTs envelopes to the chat internal endpoint with a
// service-to-service bearer token.
type httpPublisher struct {
	endpoint string
	token    string
	client   *http.Client
	log      *slog.Logger
}

// NewPublisher builds the chat event publisher. An empty token selects the
// disabled publisher (dev default — events are dropped, loudly logged).
// With a token set, baseURL must be a valid http(s) URL; a bad URL is a
// configuration bug and NewPublisher returns an error so the caller can
// refuse to boot (fail-closed) rather than silently drop events.
func NewPublisher(baseURL, token string, log *slog.Logger) (Publisher, error) {
	if token == "" {
		log.Warn("realtime: no CHAT_INTERNAL_TOKEN configured — realtime events disabled (post:new / follower:new / shipment:update will not be delivered)")
		return disabledPublisher{}, nil
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("realtime: invalid CHAT_INTERNAL_URL %q: must be an http(s) URL", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + internalPath
	return &httpPublisher{
		endpoint: u.String(),
		token:    token,
		client:   &http.Client{Timeout: publishTimeout},
		log:      log,
	}, nil
}

// Publish implements Publisher.
func (p *httpPublisher) Publish(ctx context.Context, env Envelope) error {
	if err := env.validate(); err != nil {
		return err
	}
	for _, chunk := range chunkTopics(env.Topics) {
		c := env
		c.Topics = chunk
		if err := p.postOne(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (p *httpPublisher) Enabled() bool { return true }

// postOne delivers a single envelope (topics already chunked).
func (p *httpPublisher) postOne(ctx context.Context, env Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("realtime: marshal envelope: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("realtime: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("realtime: post to chat: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("realtime: chat returned %d for event %q", resp.StatusCode, env.Event)
	}
	p.log.Debug("realtime: event delivered", "event", env.Event,
		"topics", len(env.Topics), "dedupe_key", env.DedupeKey)
	return nil
}

// validate rejects malformed envelopes before any network traffic.
func (e Envelope) validate() error {
	if len(e.Topics) == 0 {
		return fmt.Errorf("realtime: envelope needs at least one topic")
	}
	for _, t := range e.Topics {
		if !strings.HasPrefix(t, "user:") || len(t) <= len("user:") {
			return fmt.Errorf("realtime: bad topic %q: must be user:<subject>", t)
		}
	}
	switch e.Event {
	case EventPostNew, EventFollowerNew, EventShipmentUpdate:
	default:
		return fmt.Errorf("realtime: unknown event %q", e.Event)
	}
	if e.DedupeKey == "" {
		return fmt.Errorf("realtime: dedupe_key is required")
	}
	return nil
}

// chunkTopics splits topics into MaxTopicsPerCall-sized batches.
func chunkTopics(topics []string) [][]string {
	var out [][]string
	for len(topics) > 0 {
		n := MaxTopicsPerCall
		if len(topics) < n {
			n = len(topics)
		}
		out = append(out, topics[:n])
		topics = topics[n:]
	}
	return out
}

// FollowerSubjects returns the OIDC subjects of everyone following
// followeeID, or nil when followeeID is a celebrity (follower count above
// threshold). The celebrity rule mirrors feed.FanoutPost exactly: Wave 1
// does not fan out realtime events for celebrity posts — their followers
// get them via home-feed pull, and the contract documents this. Keeping
// the two checks in lockstep is what makes "201 means visible" stay true
// for push-path posts: fan-out and realtime agree on the recipient set.
func FollowerSubjects(ctx context.Context, pool *db.Pool, followeeID pgtype.UUID, fanoutThreshold int) ([]string, error) {
	var count int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM follows WHERE followee_id = $1`, followeeID).Scan(&count); err != nil {
		return nil, fmt.Errorf("realtime: count followers: %w", err)
	}
	if count > int64(fanoutThreshold) {
		return nil, nil // celebrity: no realtime fan-out, same as feed
	}
	rows, err := pool.Query(ctx,
		`SELECT u.subject FROM follows f JOIN users u ON u.id = f.follower_id
		 WHERE f.followee_id = $1`, followeeID)
	if err != nil {
		return nil, fmt.Errorf("realtime: list follower subjects: %w", err)
	}
	defer rows.Close()
	var subjects []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("realtime: scan follower subject: %w", err)
		}
		subjects = append(subjects, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("realtime: list follower subjects: %w", err)
	}
	return subjects, nil
}

// subjectByID resolves a user row's OIDC subject (the chat topic key).
func subjectByID(ctx context.Context, pool *db.Pool, id pgtype.UUID) (string, error) {
	var subject string
	if err := pool.QueryRow(ctx, `SELECT subject FROM users WHERE id = $1`, id).Scan(&subject); err != nil {
		return "", fmt.Errorf("realtime: subject for user: %w", err)
	}
	return subject, nil
}

// handleOf returns the claimed handle, or nil when the user never claimed
// one (handles live on profiles; users may exist without them).
func handleOf(ctx context.Context, pool *db.Pool, id pgtype.UUID) *string {
	prof, err := social.GetProfile(ctx, pool, id)
	if err != nil || prof == nil {
		return nil
	}
	return prof.Handle
}

// PublishPostNew emits EventPostNew to the author's followers' user topics.
// It is the realtime twin of feed fan-out: same celebrity policy, same
// recipient set. Errors are returned to the caller, which must treat them
// as best-effort (log, never fail the request).
func PublishPostNew(ctx context.Context, pool *db.Pool, p *posts.Post, fanoutThreshold int, rt Publisher) error {
	if !rt.Enabled() {
		return nil
	}
	var authorID pgtype.UUID
	if err := authorID.Scan(p.AuthorID); err != nil {
		return fmt.Errorf("realtime: bad author id: %w", err)
	}
	subjects, err := FollowerSubjects(ctx, pool, authorID, fanoutThreshold)
	if err != nil {
		return err
	}
	if len(subjects) == 0 {
		return nil // no followers, or celebrity — nothing to send
	}
	topics := make([]string, len(subjects))
	for i, s := range subjects {
		topics[i] = "user:" + s
	}
	return rt.Publish(ctx, Envelope{
		Topics: topics,
		Event:  EventPostNew,
		Payload: PostNewPayload{
			PostID:       p.ID,
			AuthorID:     p.AuthorID,
			AuthorHandle: handleOf(ctx, pool, authorID),
			Text:         p.Text,
			MediaIDs:     p.MediaIDs,
			CreatedAt:    p.CreatedAt,
		},
		DedupeKey: "post:" + p.ID,
	})
}

// PublishFollowerNew emits EventFollowerNew to the followee's user topic
// after a NEW follow edge is created (idempotent retries of an existing
// edge must not notify again — the caller gates on isNew).
func PublishFollowerNew(ctx context.Context, pool *db.Pool, follower, followee pgtype.UUID, rt Publisher) error {
	if !rt.Enabled() {
		return nil
	}
	followeeSubject, err := subjectByID(ctx, pool, followee)
	if err != nil {
		return err
	}
	return rt.Publish(ctx, Envelope{
		Topics: []string{"user:" + followeeSubject},
		Event:  EventFollowerNew,
		Payload: FollowerNewPayload{
			FollowerID:     follower.String(),
			FollowerHandle: handleOf(ctx, pool, follower),
			CreatedAt:      time.Now().UTC(),
		},
		DedupeKey: "follow:" + follower.String() + ":" + followee.String(),
	})
}

// PublishShipmentUpdate emits EventShipmentUpdate to the BUYER's user
// topic after a shipment's lifecycle status (or its COD money) changes.
// The F6 saga orchestrator is the producer: it publishes after the DB
// write commits, never before, so an event always describes committed
// state. The dedupe key is (shipment, status, updated-at-second): a
// retried saga step re-publishes the same key and clients dedupe on it
// (Wave 1 clients carry the key in local state; Wave 2's inbox dedupes
// server-side). Errors are best-effort — returned to the saga, never to
// an end-user request.
func PublishShipmentUpdate(ctx context.Context, pool *db.Pool, buyerID pgtype.UUID, p ShipmentUpdatePayload, rt Publisher) error {
	if !rt.Enabled() {
		return nil
	}
	if p.ShipmentID == "" || p.OrderID == "" || p.Status == "" {
		return fmt.Errorf("realtime: shipment payload needs shipment_id, order_id and status")
	}
	subject, err := subjectByID(ctx, pool, buyerID)
	if err != nil {
		return err
	}
	return rt.Publish(ctx, Envelope{
		Topics:    []string{"user:" + subject},
		Event:     EventShipmentUpdate,
		Payload:   p,
		DedupeKey: "shipment:" + p.ShipmentID + ":" + p.Status + ":" + strconv.FormatInt(p.UpdatedAt.Unix(), 10),
	})
}
