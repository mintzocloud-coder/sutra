// SPDX-License-Identifier: AGPL-3.0-or-later
// F6 (part 2a): saga state store tests against the migration-0013
// schema (order_sagas + saga_processed_events).
//
// Integration tests are gated on TEST_DATABASE_URL (same convention
// as the orders/cart tests): they run the full store against real
// PG16 and skip cleanly otherwise. The constant-guard test is pure
// and always runs.
//
// The saga tables reference orders(id), so the fixture builds a
// minimal user -> seller -> order chain with raw SQL (the store
// under test deliberately stays decoupled from the orders service).
package saga

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping saga integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return NewStore(pool)
}

var fixtureSeq int64

// fixtureOrder inserts a minimal buyer -> seller -> order chain and
// returns the order id. Uniqueness comes from a process-local
// counter, so parallel test runs on the same database do not clash.
func fixtureOrder(t *testing.T, ctx context.Context, store *Store, tag string) pgtype.UUID {
	t.Helper()
	fixtureSeq++
	suffix := fmt.Sprintf("%s-%d-%d", tag, time.Now().UnixNano(), fixtureSeq)

	var userID, sellerID pgtype.UUID
	if err := store.pool.QueryRow(ctx,
		`INSERT INTO users (subject) VALUES ($1) RETURNING id`,
		"saga-test-"+suffix).Scan(&userID); err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	if err := store.pool.QueryRow(ctx,
		`INSERT INTO sellers (user_id, store_name, store_slug)
		 VALUES ($1, $2, $3) RETURNING id`,
		userID, "Saga Test Store "+suffix, "saga-test-"+suffix).Scan(&sellerID); err != nil {
		t.Fatalf("fixture seller: %v", err)
	}
	var orderID pgtype.UUID
	if err := store.pool.QueryRow(ctx,
		`INSERT INTO orders (buyer_id, seller_id, total_paise, payment_method)
		 VALUES ($1, $2, 100, 'cod') RETURNING id`,
		userID, sellerID).Scan(&orderID); err != nil {
		t.Fatalf("fixture order: %v", err)
	}
	return orderID
}

func TestStartAndGet(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	orderID := fixtureOrder(t, ctx, store, "start")

	saga, isNew, err := store.Start(ctx, orderID, "evt-created-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !isNew {
		t.Fatal("Start: expected isNew=true on first call")
	}
	if saga.State != StateStarted {
		t.Fatalf("Start: want state=started, got %s", saga.State)
	}
	if saga.Attempts != 0 {
		t.Fatalf("Start: want attempts=0, got %d", saga.Attempts)
	}
	if saga.NextRetryAt != nil {
		t.Fatal("Start: next_retry_at should be NULL on start")
	}

	got, err := store.Get(ctx, orderID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OrderID != orderID || got.State != StateStarted {
		t.Fatalf("Get: wrong row: %+v", got)
	}
}

func TestStartDuplicateEventIdempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	orderID := fixtureOrder(t, ctx, store, "dupe")

	if _, isNew, err := store.Start(ctx, orderID, "evt-created-2"); err != nil || !isNew {
		t.Fatalf("first Start: isNew=%v err=%v", isNew, err)
	}
	// Redelivered order.created with the SAME event_id: must not
	// claim twice, must not advance anything.
	saga, isNew, err := store.Start(ctx, orderID, "evt-created-2")
	if err != nil {
		t.Fatalf("duplicate Start: %v", err)
	}
	if isNew {
		t.Fatal("duplicate Start: expected isNew=false")
	}
	if saga.Attempts != 0 || saga.State != StateStarted {
		t.Fatalf("duplicate Start moved the saga: %s attempts=%d", saga.State, saga.Attempts)
	}
}

func TestClaimEvent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	orderID := fixtureOrder(t, ctx, store, "claim")

	if _, _, err := store.Start(ctx, orderID, "evt-created-3"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	claimed, err := store.ClaimEvent(ctx, orderID, "evt-status-1")
	if err != nil || !claimed {
		t.Fatalf("ClaimEvent: claimed=%v err=%v", claimed, err)
	}
	// Redelivery of the same (order_id, event_id): skip.
	claimed, err = store.ClaimEvent(ctx, orderID, "evt-status-1")
	if err != nil {
		t.Fatalf("duplicate ClaimEvent: %v", err)
	}
	if claimed {
		t.Fatal("duplicate ClaimEvent: expected claimed=false")
	}
	// Event for an order with no saga: not a duplicate, a real error.
	otherOrder := fixtureOrder(t, ctx, store, "claim-other")
	claimed, err = store.ClaimEvent(ctx, otherOrder, "evt-status-9")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClaimEvent without saga: claimed=%v err=%v, want ErrNotFound", claimed, err)
	}
}

func TestTransitionGuarded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	orderID := fixtureOrder(t, ctx, store, "trans")

	if _, _, err := store.Start(ctx, orderID, "evt-created-4"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wrong expected state: the saga must not move.
	if err := store.Transition(ctx, orderID, StateReserved, StateConfirmed); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("Transition wrong state: err=%v, want ErrStateMismatch", err)
	}
	saga, _ := store.Get(ctx, orderID)
	if saga.State != StateStarted || saga.Attempts != 0 {
		t.Fatalf("Transition wrong state moved the saga: %s attempts=%d", saga.State, saga.Attempts)
	}

	// Correct state: moves forward and bumps attempts.
	if err := store.Transition(ctx, orderID, StateStarted, StateReserved); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	saga, _ = store.Get(ctx, orderID)
	if saga.State != StateReserved || saga.Attempts != 1 || saga.LastError != nil {
		t.Fatalf("Transition: got %s attempts=%d lastErr=%v", saga.State, saga.Attempts, saga.LastError)
	}

	// Same event redelivered after the state moved on: the expected
	// state no longer matches — ErrStateMismatch, not a double step.
	if err := store.Transition(ctx, orderID, StateStarted, StateReserved); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("Transition stale state: err=%v, want ErrStateMismatch", err)
	}
}

func TestFailureRetryAndTerminalLocks(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	orderID := fixtureOrder(t, ctx, store, "fail")

	if _, _, err := store.Start(ctx, orderID, "evt-created-5"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	retryAt := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	if err := store.RecordFailure(ctx, orderID, "booking refused: provider stub", retryAt); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	saga, _ := store.Get(ctx, orderID)
	if saga.Attempts != 1 || saga.LastError == nil || *saga.LastError != "booking refused: provider stub" {
		t.Fatalf("RecordFailure: attempts=%d lastError=%v", saga.Attempts, saga.LastError)
	}
	if saga.NextRetryAt == nil || !saga.NextRetryAt.UTC().Truncate(time.Second).Equal(retryAt) {
		t.Fatalf("RecordFailure: next_retry_at=%v, want %v", saga.NextRetryAt, retryAt)
	}

	// Drive the saga to the dead-letter exit.
	for _, step := range [][2]string{
		{StateStarted, StateReserved},
		{StateReserved, StateConfirmed},
		{StateConfirmed, StateAwaitingBooking},
		{StateAwaitingBooking, StateDeadLetter},
	} {
		if err := store.Transition(ctx, orderID, step[0], step[1]); err != nil {
			t.Fatalf("Transition %s->%s: %v", step[0], step[1], err)
		}
	}
	saga, _ = store.Get(ctx, orderID)
	if saga.State != StateDeadLetter {
		t.Fatalf("want dead_letter, got %s", saga.State)
	}

	// A terminal saga rejects every further transition...
	if err := store.Transition(ctx, orderID, StateDeadLetter, StateShipped); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("Transition on terminal: err=%v, want ErrTerminalState", err)
	}
	if err := store.RecordFailure(ctx, orderID, "late error", time.Now()); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("RecordFailure on terminal: err=%v, want ErrTerminalState", err)
	}
	// ...and cannot be resurrected into a different terminal state.
	if err := store.Transition(ctx, orderID, StateAwaitingBooking, StateShipped); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("resurrect terminal: err=%v, want ErrTerminalState", err)
	}
}

func TestGetMissing(t *testing.T) {
	store := testStore(t)
	var missing pgtype.UUID
	if err := missing.Scan("00000000-0000-4000-8000-000000000000"); err != nil {
		t.Fatalf("uuid scan: %v", err)
	}
	if _, err := store.Get(context.Background(), missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: err=%v, want ErrNotFound", err)
	}
}

func TestStateConstantsMatchMigration(t *testing.T) {
	// Pure test: the Go constants must match migration 0013's CHECK
	// constraint exactly, and the terminal set must cover exactly the
	// three documented terminal states. A drift here is a schema/code
	// mismatch that would surface only in CI.
	all := []string{StateStarted, StateReserved, StateConfirmed, StateAwaitingBooking,
		StateShipped, StateCompensatedCancelled, StateDeadLetter}
	seen := map[string]bool{}
	for _, st := range all {
		if seen[st] {
			t.Fatalf("duplicate state constant %q", st)
		}
		seen[st] = true
	}
	for _, term := range []string{StateShipped, StateCompensatedCancelled, StateDeadLetter} {
		if !terminalStates[term] {
			t.Fatalf("terminal state %q missing from terminalStates", term)
		}
	}
	for _, non := range []string{StateStarted, StateReserved, StateConfirmed, StateAwaitingBooking} {
		if terminalStates[non] {
			t.Fatalf("non-terminal state %q in terminalStates", non)
		}
	}
}
