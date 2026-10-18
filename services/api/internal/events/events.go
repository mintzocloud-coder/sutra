// SPDX-License-Identifier: AGPL-3.0-or-later
// Package events is the Go side of the Wave-3 event bus (F6 part 1):
// order lifecycle events published over NATS JetStream.
//
// DESIGN: events are notifications, never the system of record —
// Postgres is. Order writes (checkout, status transitions) stay
// 201/200 even when NATS is down: the API's hooks log a WARN and move
// on, and the saga orchestrator (F6 part 2) reconciles from the
// database. A NATS_URL that is set but unreachable is a configuration
// bug — refuse to boot (fail closed) so operators notice instead of
// silently dropping every order event.
//
// Stream layout (ADR-001): one stream "sutra" with subject prefix
// "sutra.orders.". JetStream gives durable, per-subject-ordered,
// replayable delivery with message-id dedupe (24h window), which is
// what the saga orchestrator and the Python AI service consume from.
//
// Subjects today:
//
//	sutra.orders.created         — one per order from Checkout
//	sutra.orders.status_changed  — one per order status transition
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

// StreamName is the single JetStream stream for the marketplace.
// Subjects are namespaced per domain so future waves (chat, feed)
// can share the stream without a migration.
const StreamName = "sutra"

// Order event names (the "event" field of the envelope).
const (
	// OrderCreated fires once per order written by Checkout — one
	// order per seller, so a multi-seller checkout emits several.
	OrderCreated = "order.created"
	// OrderStatusChanged fires after every successful order status
	// transition (confirmed, shipped, delivered, cancelled, returned).
	OrderStatusChanged = "order.status_changed"
)

// eventSubjects maps the event name to its JetStream subject. The
// subject keeps the verb only — the "orders." prefix already scopes
// the domain.
var eventSubjects = map[string]string{
	OrderCreated:       "sutra.orders.created",
	OrderStatusChanged: "sutra.orders.status_changed",
}

// subjectFor returns the JetStream subject for an event name, or ""
// when the name is unknown.
func subjectFor(event string) string {
	return eventSubjects[event]
}

// NewEventID mints a unique event id. It doubles as the JetStream
// message id (Nats-Msg-Id) so a retried publish dedupes instead of
// double-delivering.
func NewEventID() string { return nuid.Next() }

// OrderEvent is the envelope for every order lifecycle event. All
// ids are uuid strings; money is integer paise; timestamps are unix
// seconds. Version 1 — bump only with a consumer-side migration.
type OrderEvent struct {
	Event         string `json:"event"`
	EventID       string `json:"event_id"`
	OrderID       string `json:"order_id"`
	BuyerID       string `json:"buyer_id"`
	SellerID      string `json:"seller_id"`
	Status        string `json:"status"`
	TotalPaise    int64  `json:"total_paise"`
	PaymentMethod string `json:"payment_method"`
	OccurredAt    int64  `json:"occurred_at"`
	Version       int    `json:"version"`
}

// Validate rejects malformed envelopes before they reach the bus —
// a consumer should never have to guess whether order_id is present.
func (e OrderEvent) Validate() error {
	switch e.Event {
	case OrderCreated, OrderStatusChanged:
	default:
		return fmt.Errorf("events: unknown event %q", e.Event)
	}
	if e.EventID == "" {
		return fmt.Errorf("events: event_id is required")
	}
	if e.OrderID == "" {
		return fmt.Errorf("events: order_id is required")
	}
	if e.BuyerID == "" {
		return fmt.Errorf("events: buyer_id is required")
	}
	if e.SellerID == "" {
		return fmt.Errorf("events: seller_id is required")
	}
	if e.Status == "" {
		return fmt.Errorf("events: status is required")
	}
	if e.OccurredAt <= 0 {
		return fmt.Errorf("events: occurred_at must be a positive unix timestamp")
	}
	if e.Version != 1 {
		return fmt.Errorf("events: unsupported version %d", e.Version)
	}
	return nil
}

// EventBus publishes order events. The disabled bus no-ops; the
// JetStream bus fails its publish calls when NATS is unreachable so
// the caller can log and move on (the write already committed).
type EventBus interface {
	Enabled() bool
	PublishOrderEvent(ctx context.Context, ev OrderEvent) error
	Close() error
}

// publishTimeout bounds one JetStream publish. The API invokes the
// bus from a detached goroutine with its own 15s deadline; the bus
// timeout is the inner bound so a hung NATS cannot pile up writers.
const publishTimeout = 5 * time.Second

// connectBudget bounds the boot-time dial: NATS may still be
// starting when the API boots (compose starts them together), so
// retry briefly — but refuse to boot if it never appears.
var connectBudget = 10 * time.Second

// NewBus builds the event bus. An empty URL DISABLES the bus (dev
// default — publishes become no-ops and a loud WARN is logged). A
// URL that cannot be dialled is a configuration bug: fail closed so
// operators notice instead of silently dropping order events.
func NewBus(url string, log *slog.Logger) (EventBus, error) {
	if url == "" {
		log.Warn("events: no NATS_URL configured — order event bus disabled (saga orchestrator will see no events)")
		return &disabledBus{log: log}, nil
	}
	if !strings.HasPrefix(url, "nats://") && !strings.HasPrefix(url, "tls://") {
		return nil, fmt.Errorf("events: invalid NATS_URL %q: must be nats:// or tls://", redactURL(url))
	}

	var nc *nats.Conn
	var err error
	deadline := time.Now().Add(connectBudget)
	for {
		nc, err = nats.Connect(url,
			nats.Name("sutra-api"),
			nats.Timeout(2*time.Second),
			nats.MaxReconnects(-1),
		)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("events: cannot reach NATS at %q: %w", redactURL(url), err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	js, err := nc.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("events: JetStream unavailable: %w", err)
	}
	if err := ensureStream(js); err != nil {
		nc.Close()
		return nil, err
	}
	return &jetstreamBus{nc: nc, js: js, log: log}, nil
}

// ensureStream creates the sutra stream once; an existing stream is
// fine (name collision is the expected steady state, not an error).
func ensureStream(js nats.JetStreamContext) error {
	_, err := js.AddStream(&nats.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{"sutra.orders.>"},
		Duplicates: 24 * time.Hour, // retried publishes dedupe for a day
	})
	if err == nil || err == nats.ErrStreamNameAlreadyInUse {
		return nil
	}
	// The stream may exist with a different config (e.g. created by
	// an older build); that is acceptable — read it to confirm.
	if _, serr := js.StreamInfo(StreamName); serr == nil {
		return nil
	}
	return fmt.Errorf("events: cannot ensure JetStream stream %q: %w", StreamName, err)
}

// redactURL strips credentials from a NATS url for log/error output.
func redactURL(url string) string {
	if i := strings.LastIndex(url, "@"); i >= 0 {
		if j := strings.Index(url, "://"); j >= 0 {
			return url[:j+3] + "***" + url[i:]
		}
	}
	return url
}

// disabledBus is the no-op bus used when NATS_URL is empty.
type disabledBus struct{ log *slog.Logger }

func (b *disabledBus) Enabled() bool { return false }

func (b *disabledBus) PublishOrderEvent(_ context.Context, _ OrderEvent) error { return nil }

func (b *disabledBus) Close() error { return nil }

// jetstreamBus publishes to the sutra stream. It is safe for
// concurrent use — nats.Conn and JetStreamContext both are.
type jetstreamBus struct {
	nc  *nats.Conn
	js  nats.JetStreamContext
	log *slog.Logger

	mu     sync.Mutex
	closed bool
}

func (b *jetstreamBus) Enabled() bool { return true }

// PublishOrderEvent validates the envelope, then publishes it on the
// event's subject with the event id as the JetStream message id
// (retried publishes dedupe within the 24h window instead of
// double-delivering to the saga orchestrator).
func (b *jetstreamBus) PublishOrderEvent(ctx context.Context, ev OrderEvent) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	subj := subjectFor(ev.Event)
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("events: marshal order event: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	msg := &nats.Msg{Subject: subj, Data: body, Header: nats.Header{}}
	msg.Header.Set("Nats-Msg-Id", ev.EventID)
	if _, err := b.js.PublishMsg(msg, nats.Context(cctx)); err != nil {
		return fmt.Errorf("events: publish %s: %w", subj, err)
	}
	return nil
}

func (b *jetstreamBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.nc.Close()
	return nil
}
