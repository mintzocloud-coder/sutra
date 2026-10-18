// SPDX-License-Identifier: AGPL-3.0-or-later
package saga

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/events"
	"github.com/mintzocloud-coder/sutra/services/api/internal/orders"
)

// fakeSagaStore is a scriptable SagaStore: no Postgres, no JetStream.
type fakeSagaStore struct {
	startNew   bool
	startErr   error
	claimed    bool
	claimErr   error
	transErr   error
	getSaga    Saga
	getErr     error
	failCalls  int
	transCalls [][2]string
}

func (f *fakeSagaStore) Start(_ context.Context, _ pgtype.UUID, _ string) (Saga, bool, error) {
	return f.getSaga, f.startNew, f.startErr
}
func (f *fakeSagaStore) ClaimEvent(_ context.Context, _ pgtype.UUID, _ string) (bool, error) {
	return f.claimed, f.claimErr
}
func (f *fakeSagaStore) Transition(_ context.Context, _ pgtype.UUID, from, to string) error {
	f.transCalls = append(f.transCalls, [2]string{from, to})
	return f.transErr
}
func (f *fakeSagaStore) RecordFailure(_ context.Context, _ pgtype.UUID, _ string, _ time.Time) error {
	f.failCalls++
	return nil
}
func (f *fakeSagaStore) Get(_ context.Context, _ pgtype.UUID) (Saga, error) {
	return f.getSaga, f.getErr
}

// fakeOrders scripts the orders service surface the engine uses.
type fakeOrders struct {
	ord        *orders.Order
	getErr     error
	transErr   error
	transCalls []string
	getCalls   int
}

func (f *fakeOrders) GetSystem(_ context.Context, _ pgtype.UUID) (*orders.Order, error) {
	f.getCalls++
	return f.ord, f.getErr
}
func (f *fakeOrders) TransitionSystem(_ context.Context, _ pgtype.UUID, to string) (*orders.Order, error) {
	f.transCalls = append(f.transCalls, to)
	if f.transErr != nil {
		return nil, f.transErr
	}
	cp := *f.ord
	cp.Status = to
	f.ord = &cp
	return f.ord, nil
}

// fakeInv scripts the inventory read.
type fakeInv struct {
	quantity int
	reserved int
	err      error
}

func (f *fakeInv) GetInventory(_ context.Context, variantID pgtype.UUID) (*catalog.Inventory, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &catalog.Inventory{
		VariantID: variantID.String(),
		Quantity:  f.quantity,
		Reserved:  f.reserved,
		Available: f.quantity - f.reserved,
	}, nil
}

func testOrder(status string) *orders.Order {
	return &orders.Order{
		ID:            "00000000-0000-0000-0000-000000000001",
		BuyerID:       "00000000-0000-0000-0000-000000000002",
		SellerID:      "00000000-0000-0000-0000-000000000003",
		Status:        status,
		PaymentMethod: "cod",
		TotalPaise:    99800,
		Items: []*orders.Item{{
			VariantID:  "00000000-0000-0000-0000-000000000004",
			Quantity:   2,
			PricePaise: 49900,
		}},
	}
}

func testEvent(event, status string) events.OrderEvent {
	return events.OrderEvent{
		Event:         event,
		EventID:       "evt-1",
		OrderID:       "00000000-0000-0000-0000-000000000001",
		BuyerID:       "00000000-0000-0000-0000-000000000002",
		SellerID:      "00000000-0000-0000-0000-000000000003",
		Status:        status,
		TotalPaise:    99800,
		PaymentMethod: "cod",
		OccurredAt:    time.Now().Unix(),
		Version:       1,
	}
}

func newTestEngine(store *fakeSagaStore, ord *orders.Order, inv *fakeInv) (*Engine, *fakeOrders, *fakeSagaStore) {
	fo := &fakeOrders{ord: ord}
	return NewEngine(store, fo, inv, slog.New(slog.NewTextHandler(io.Discard, nil))), fo, store
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Happy path: order.created → verify → confirm → saga confirmed.
func TestHandleCreated_FullDrive(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{quantity: 10, reserved: 5})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if !equalStrings(fo.transCalls, []string{orders.StatusConfirmed}) {
		t.Errorf("order transitions = %v, want [confirmed]", fo.transCalls)
	}
	if len(st.transCalls) != 2 ||
		st.transCalls[0] != [2]string{StateStarted, StateReserved} ||
		st.transCalls[1] != [2]string{StateReserved, StateConfirmed} {
		t.Errorf("saga transitions = %v, want started→reserved→confirmed", st.transCalls)
	}
	if st.failCalls != 0 {
		t.Errorf("failCalls = %d, want 0", st.failCalls)
	}
}

// Redelivered order.created: Start says not-new → skip everything.
func TestHandleCreated_DuplicateEvent(t *testing.T) {
	store := &fakeSagaStore{startNew: false}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{quantity: 10, reserved: 5})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(fo.transCalls) != 0 || len(st.transCalls) != 0 || fo.getCalls != 0 {
		t.Errorf("duplicate event caused work: order=%v saga=%v gets=%d",
			fo.transCalls, st.transCalls, fo.getCalls)
	}
}

// Broken reservation: compensate (cancel order) and park the saga.
func TestHandleCreated_ReservationBroken_Compensates(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{quantity: 10, reserved: 1})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if !equalStrings(fo.transCalls, []string{orders.StatusCancelled}) {
		t.Errorf("order transitions = %v, want [cancelled]", fo.transCalls)
	}
	if len(st.transCalls) != 1 || st.transCalls[0] != [2]string{StateStarted, StateCompensatedCancelled} {
		t.Errorf("saga transitions = %v, want started→compensated_cancelled", st.transCalls)
	}
}

// Missing inventory row is broken (not transient): compensate.
func TestHandleCreated_InventoryGone_Compensates(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCreated),
		&fakeInv{err: catalog.ErrVariantNotFound})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if !equalStrings(fo.transCalls, []string{orders.StatusCancelled}) {
		t.Errorf("order transitions = %v, want [cancelled]", fo.transCalls)
	}
	if len(st.transCalls) != 1 || st.transCalls[0] != [2]string{StateStarted, StateCompensatedCancelled} {
		t.Errorf("saga transitions = %v, want started→compensated_cancelled", st.transCalls)
	}
}

// Transient inventory failure: RecordFailure + error (redeliver).
func TestHandleCreated_InventoryTransient_Retries(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, _, st := newTestEngine(store, testOrder(orders.StatusCreated),
		&fakeInv{err: errors.New("db down")})

	err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated))
	if err == nil {
		t.Fatal("want error for transient failure, got nil")
	}
	if st.failCalls != 1 {
		t.Errorf("failCalls = %d, want 1", st.failCalls)
	}
	if len(st.transCalls) != 0 {
		t.Errorf("saga transitions = %v, want none", st.transCalls)
	}
}

// Order died before the saga started: park, don't touch the order.
func TestHandleCreated_OrderAlreadyCancelled(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCancelled), &fakeInv{quantity: 10, reserved: 5})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCancelled)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(fo.transCalls) != 0 {
		t.Errorf("order transitions = %v, want none", fo.transCalls)
	}
	if len(st.transCalls) != 1 || st.transCalls[0] != [2]string{StateStarted, StateCompensatedCancelled} {
		t.Errorf("saga transitions = %v, want started→compensated_cancelled", st.transCalls)
	}
}

// Seller confirmed concurrently before the saga read the order:
// confirm is a no-op, saga still advances.
func TestHandleCreated_SellerConfirmedRace(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusConfirmed), &fakeInv{quantity: 10, reserved: 5})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusConfirmed)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(fo.transCalls) != 0 {
		t.Errorf("order transitions = %v, want none (already confirmed)", fo.transCalls)
	}
	if len(st.transCalls) != 2 ||
		st.transCalls[1] != [2]string{StateReserved, StateConfirmed} {
		t.Errorf("saga transitions = %v, want started→reserved→confirmed", st.transCalls)
	}
}

// Seller confirms between our read and our transition: the
// ErrInvalidTransition is absorbed after re-reading.
func TestHandleCreated_ConfirmRaceInvalidTransition(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, fo, st := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{quantity: 10, reserved: 5})
	eng.orders = &raceOrders{fake: fo, onFirst: func() {
		// Simulate the seller confirming between our Get and our
		// TransitionSystem: the order is now confirmed.
		fo.ord.Status = orders.StatusConfirmed
	}}

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(fo.transCalls) != 0 {
		t.Errorf("order transitions = %v, want none (race absorbed)", fo.transCalls)
	}
	if len(st.transCalls) != 2 || st.transCalls[1] != [2]string{StateReserved, StateConfirmed} {
		t.Errorf("saga transitions = %v, want started→reserved→confirmed", st.transCalls)
	}
}

// raceOrders wraps fakeOrders to inject a one-shot race.
type raceOrders struct {
	fake    *fakeOrders
	onFirst func()
	hit     bool
}

func (r *raceOrders) GetSystem(ctx context.Context, id pgtype.UUID) (*orders.Order, error) {
	return r.fake.GetSystem(ctx, id)
}
func (r *raceOrders) TransitionSystem(ctx context.Context, id pgtype.UUID, to string) (*orders.Order, error) {
	if !r.hit {
		r.hit = true
		r.onFirst()
		return nil, orders.ErrInvalidTransition
	}
	return r.fake.TransitionSystem(ctx, id, to)
}

// Transient order read failure: RecordFailure + error (redeliver).
func TestHandleCreated_LoadTransient_Retries(t *testing.T) {
	store := &fakeSagaStore{startNew: true}
	eng, _, st := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{quantity: 10, reserved: 5})
	eng.orders = &fakeOrders{getErr: errors.New("db down")}

	err := eng.HandleEvent(context.Background(), testEvent(events.OrderCreated, orders.StatusCreated))
	if err == nil {
		t.Fatal("want error for transient failure, got nil")
	}
	if st.failCalls != 1 {
		t.Errorf("failCalls = %d, want 1", st.failCalls)
	}
}

// External cancel: saga parks in compensated_cancelled.
func TestHandleStatusChanged_ExternalCancel(t *testing.T) {
	store := &fakeSagaStore{claimed: true, getSaga: Saga{State: StateConfirmed}}
	eng, _, st := newTestEngine(store, testOrder(orders.StatusConfirmed), &fakeInv{})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderStatusChanged, orders.StatusCancelled)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(st.transCalls) != 1 || st.transCalls[0] != [2]string{StateConfirmed, StateCompensatedCancelled} {
		t.Errorf("saga transitions = %v, want confirmed→compensated_cancelled", st.transCalls)
	}
}

// Duplicate status_changed: skip.
func TestHandleStatusChanged_Duplicate(t *testing.T) {
	store := &fakeSagaStore{claimed: false}
	eng, _, st := newTestEngine(store, testOrder(orders.StatusConfirmed), &fakeInv{})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderStatusChanged, orders.StatusCancelled)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(st.transCalls) != 0 {
		t.Errorf("saga transitions = %v, want none", st.transCalls)
	}
}

// No saga for the order (pre-saga order): skip silently.
func TestHandleStatusChanged_NoSaga(t *testing.T) {
	store := &fakeSagaStore{claimed: false, claimErr: ErrNotFound}
	eng, _, _ := newTestEngine(store, testOrder(orders.StatusConfirmed), &fakeInv{})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderStatusChanged, orders.StatusCancelled)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
}

// Irrelevant status (e.g. shipped, informational in v1): skip.
func TestHandleStatusChanged_IrrelevantStatus(t *testing.T) {
	store := &fakeSagaStore{claimed: true}
	eng, _, st := newTestEngine(store, testOrder(orders.StatusConfirmed), &fakeInv{})

	if err := eng.HandleEvent(context.Background(), testEvent(events.OrderStatusChanged, orders.StatusShipped)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(st.transCalls) != 0 {
		t.Errorf("saga transitions = %v, want none", st.transCalls)
	}
}

// Benign races on advance are swallowed.
func TestAdvance_IdempotentRaces(t *testing.T) {
	for _, race := range []error{ErrStateMismatch, ErrTerminalState} {
		store := &fakeSagaStore{startNew: true, transErr: race}
		eng, _, _ := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{})
		var u pgtype.UUID
		if err := eng.advance(context.Background(), u, StateStarted, StateReserved); err != nil {
			t.Errorf("advance with %v: %v", race, err)
		}
	}
}

// Real transition errors propagate.
func TestAdvance_PropagatesErrors(t *testing.T) {
	store := &fakeSagaStore{transErr: errors.New("db down")}
	eng, _, _ := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{})
	var u pgtype.UUID
	if err := eng.advance(context.Background(), u, StateStarted, StateReserved); err == nil {
		t.Error("want error, got nil")
	}
}

// Backoff: 30s doubling, capped at 30m.
func TestBackoffFor(t *testing.T) {
	cases := []struct {
		attempts int32
		want     time.Duration
	}{
		{0, 30 * time.Second},
		{1, time.Minute},
		{3, 4 * time.Minute},
		{10, 30 * time.Minute}, // capped
		{100, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := backoffFor(c.attempts); got != c.want {
			t.Errorf("backoffFor(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

// Bad envelope → error, never a panic.
func TestHandleEvent_BadEnvelope(t *testing.T) {
	store := &fakeSagaStore{}
	eng, _, _ := newTestEngine(store, testOrder(orders.StatusCreated), &fakeInv{})
	ev := testEvent(events.OrderCreated, orders.StatusCreated)
	ev.OrderID = "not-a-uuid"
	if err := eng.HandleEvent(context.Background(), ev); err == nil {
		t.Error("want error for bad order_id, got nil")
	}
}
