// SPDX-License-Identifier: AGPL-3.0-or-later
// Consumer side of the Wave-3 event bus (F6 part 2b): the order saga
// orchestrator's durable JetStream consumer.
//
// The consumer is a pull subscription on "sutra.orders.>" with a
// durable name (default "saga-orchestrator"): JetStream tracks the
// delivery position server-side, so a restarted worker resumes where
// it left off and redelivers only unacknowledged messages
// (AckExplicit). Delivery ordering is per subject; the saga engine
// (internal/saga) is idempotent in (order_id, event_id), so
// redeliveries and cross-subject out-of-order events are safe.
//
// Poison messages (undecodable bodies or invalid envelopes) are
// acknowledged and logged, never redelivered: a malformed event can
// never wedge the stream. Handler errors NAK the message for
// redelivery per the consumer's AckWait/MaxDeliver policy.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// SagaDurableName is the durable consumer name for the order saga
// orchestrator. One logical consumer group in v1: a single worker
// keeps per-subject ordering simple, and the engine's idempotent
// steps make exactly-once processing unnecessary. Scale-out arrives
// with the reconciler (F6 part 3).
const SagaDurableName = "saga-orchestrator"

// EventHandler consumes one validated order event. A nil return
// acknowledges the message; a non-nil return redelivers it.
type EventHandler interface {
	HandleEvent(ctx context.Context, ev OrderEvent) error
}

// ConsumerConfig tunes the durable subscription.
type ConsumerConfig struct {
	// Durable is the JetStream durable name; defaults to
	// SagaDurableName.
	Durable string
	// AckWait bounds how long the server waits for an ack before
	// redelivery. Defaults to 30s.
	AckWait time.Duration
	// MaxDeliver caps redeliveries before the server stops retrying.
	// Defaults to 10.
	MaxDeliver int
	// Batch is the pull batch size. Defaults to 16.
	Batch int
}

func (c ConsumerConfig) withDefaults() ConsumerConfig {
	if c.Durable == "" {
		c.Durable = SagaDurableName
	}
	if c.AckWait <= 0 {
		c.AckWait = 30 * time.Second
	}
	if c.MaxDeliver <= 0 {
		c.MaxDeliver = 10
	}
	if c.Batch <= 0 {
		c.Batch = 16
	}
	return c
}

// Consumer is the durable JetStream subscription. Build it with
// NewConsumer, then Run it with a Handler.
type Consumer struct {
	nc       *nats.Conn
	js       nats.JetStreamContext
	log      *slog.Logger
	disabled bool
}

// NewConsumer connects for consuming. An empty URL disables the
// consumer (dev default — Run returns immediately with a loud WARN).
// A URL that cannot be dialled is a configuration bug: fail closed,
// the same contract as NewBus.
func NewConsumer(url string, log *slog.Logger) (*Consumer, error) {
	if url == "" {
		log.Warn("events: no NATS_URL configured — saga consumer disabled (order events will not be processed)")
		return &Consumer{disabled: true, log: log}, nil
	}
	nc, js, err := connectJetStream(url, "sutra-saga", log)
	if err != nil {
		return nil, err
	}
	return &Consumer{nc: nc, js: js, log: log}, nil
}

// Close releases the NATS connection.
func (c *Consumer) Close() error {
	if c.disabled {
		return nil
	}
	c.nc.Close()
	return nil
}

// Run blocks until ctx is cancelled, pulling order events and
// dispatching them to h. It returns ctx.Err() on cancellation.
func (c *Consumer) Run(ctx context.Context, cfg ConsumerConfig, h EventHandler) error {
	if c.disabled {
		c.log.Warn("events: saga consumer disabled — no events will be processed")
		return nil
	}
	cfg = cfg.withDefaults()
	sub, err := c.js.PullSubscribe("sutra.orders.>", cfg.Durable,
		nats.DeliverAll(), // replay history on first durable creation
		nats.AckExplicit(),
		nats.AckWait(cfg.AckWait),
		nats.MaxDeliver(cfg.MaxDeliver),
		nats.ManualAck(),
	)
	if err != nil {
		return fmt.Errorf("events: pull subscribe: %w", err)
	}
	c.log.Info("events: saga consumer running", "durable", cfg.Durable)
	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			return ctx.Err()
		default:
		}
		msgs, err := sub.Fetch(cfg.Batch, nats.MaxWait(5*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue // idle stream: poll again
			}
			// Fetch errors are usually transient (reconnect churn);
			// log and keep the loop alive rather than killing the
			// worker over a blip.
			c.log.Warn("events: fetch failed, retrying", "error", err)
			time.Sleep(time.Second)
			continue
		}
		for _, m := range msgs {
			c.dispatch(ctx, m, h)
		}
	}
}

// dispatch handles one message: poison (undecodable/invalid) is
// acked+logged, handler failure is nacked for redelivery, success is
// acked. AckSync waits for the server's confirmation so an ack is
// never silently lost.
func (c *Consumer) dispatch(ctx context.Context, m *nats.Msg, h EventHandler) {
	var ev OrderEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		c.log.Error("events: poison message (undecodable), acking",
			"subject", m.Subject, "error", err)
		if aerr := m.AckSync(nats.Context(ctx)); aerr != nil {
			c.log.Error("events: ack of poison message failed", "error", aerr)
		}
		return
	}
	if err := ev.Validate(); err != nil {
		c.log.Error("events: poison message (invalid envelope), acking",
			"subject", m.Subject, "error", err)
		if aerr := m.AckSync(nats.Context(ctx)); aerr != nil {
			c.log.Error("events: ack of poison message failed", "error", aerr)
		}
		return
	}
	if err := h.HandleEvent(ctx, ev); err != nil {
		c.log.Warn("events: handler failed, naking for redelivery",
			"event", ev.Event, "order_id", ev.OrderID, "error", err)
		if nerr := m.Nak(nats.Context(ctx)); nerr != nil {
			c.log.Error("events: nak failed", "error", nerr)
		}
		return
	}
	if err := m.AckSync(nats.Context(ctx)); err != nil {
		c.log.Error("events: ack failed",
			"event", ev.Event, "order_id", ev.OrderID, "error", err)
	}
}
