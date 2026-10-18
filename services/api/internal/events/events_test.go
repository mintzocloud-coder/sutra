// SPDX-License-Identifier: AGPL-3.0-or-later
package events

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validEvent() OrderEvent {
	return OrderEvent{
		Event:         OrderCreated,
		EventID:       NewEventID(),
		OrderID:       "ord-1",
		BuyerID:       "buyer-1",
		SellerID:      "seller-1",
		Status:        "created",
		TotalPaise:    19900,
		PaymentMethod: "cod",
		OccurredAt:    time.Now().Unix(),
		Version:       1,
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*OrderEvent)
		wantErr string
	}{
		{"valid", func(*OrderEvent) {}, ""},
		{"status_changed valid", func(e *OrderEvent) { e.Event = OrderStatusChanged; e.Status = "shipped" }, ""},
		{"unknown event", func(e *OrderEvent) { e.Event = "order.teleported" }, "unknown event"},
		{"empty event", func(e *OrderEvent) { e.Event = "" }, "unknown event"},
		{"empty event_id", func(e *OrderEvent) { e.EventID = "" }, "event_id"},
		{"empty order_id", func(e *OrderEvent) { e.OrderID = "" }, "order_id"},
		{"empty buyer_id", func(e *OrderEvent) { e.BuyerID = "" }, "buyer_id"},
		{"empty seller_id", func(e *OrderEvent) { e.SellerID = "" }, "seller_id"},
		{"empty status", func(e *OrderEvent) { e.Status = "" }, "status"},
		{"zero occurred_at", func(e *OrderEvent) { e.OccurredAt = 0 }, "occurred_at"},
		{"negative occurred_at", func(e *OrderEvent) { e.OccurredAt = -5 }, "occurred_at"},
		{"version 0", func(e *OrderEvent) { e.Version = 0 }, "unsupported version"},
		{"version 2", func(e *OrderEvent) { e.Version = 2 }, "unsupported version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvent()
			tc.mutate(&ev)
			err := ev.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
			}
		})
	}
}

func TestSubjectFor(t *testing.T) {
	if got := subjectFor(OrderCreated); got != "sutra.orders.created" {
		t.Fatalf("order.created subject = %q", got)
	}
	if got := subjectFor(OrderStatusChanged); got != "sutra.orders.status_changed" {
		t.Fatalf("order.status_changed subject = %q", got)
	}
	if got := subjectFor("order.bogus"); got != "" {
		t.Fatalf("unknown event subject = %q, want empty", got)
	}
}

func TestNewEventID(t *testing.T) {
	a, b := NewEventID(), NewEventID()
	if a == "" || b == "" {
		t.Fatal("event ids must be non-empty")
	}
	if a == b {
		t.Fatal("event ids must be unique")
	}
}

func TestNewBusDisabled(t *testing.T) {
	bus, err := NewBus("", testLog())
	if err != nil {
		t.Fatalf("empty NATS_URL must not fail: %v", err)
	}
	if bus.Enabled() {
		t.Fatal("disabled bus must report Enabled() == false")
	}
	// No-op publish: even a valid event is dropped silently.
	if err := bus.PublishOrderEvent(context.Background(), validEvent()); err != nil {
		t.Fatalf("disabled publish must no-op: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("disabled close must no-op: %v", err)
	}
}

func TestNewBusBadScheme(t *testing.T) {
	if _, err := NewBus("http://nats:4222", testLog()); err == nil {
		t.Fatal("http scheme must fail closed")
	}
	if _, err := NewBus("://garbage", testLog()); err == nil {
		t.Fatal("malformed URL must fail closed")
	}
}

func TestNewBusUnreachableFailClosed(t *testing.T) {
	old := connectBudget
	connectBudget = 100 * time.Millisecond
	defer func() { connectBudget = old }()

	// 127.0.0.1:1 refuses immediately — the dial fails and the budget
	// expires, so NewBus must return an error (fail closed), never a
	// half-built bus.
	if _, err := NewBus("nats://127.0.0.1:1", testLog()); err == nil {
		t.Fatal("unreachable NATS must fail closed")
	}
}

func TestPublishValidatesBeforeTouchingWire(t *testing.T) {
	// A zero jetstreamBus has no connection; an invalid event must be
	// rejected by validation before any NATS call (no panic).
	bus := &jetstreamBus{}
	err := bus.PublishOrderEvent(context.Background(), OrderEvent{})
	if err == nil || !strings.Contains(err.Error(), "unknown event") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("nats://user:secret@nats:4222"); strings.Contains(got, "secret") {
		t.Fatalf("credentials leaked: %q", got)
	} else if !strings.Contains(got, "@nats:4222") {
		t.Fatalf("host lost: %q", got)
	}
	if got := redactURL("nats://nats:4222"); got != "nats://nats:4222" {
		t.Fatalf("plain URL changed: %q", got)
	}
}
