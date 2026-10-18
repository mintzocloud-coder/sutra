// SPDX-License-Identifier: AGPL-3.0-or-later
// Package saga is the Wave-3 order-saga state store (F6 part 2a).
//
// It implements the state machine defined by migration 0013
// (order_sagas + saga_processed_events). The saga orchestrator
// (F6 part 2b) is a durable JetStream consumer driving
// started -> reserved -> confirmed -> awaiting_booking -> shipped,
// with compensated_cancelled / dead_letter as the failure exits.
// Postgres is the system of record; NATS is notifications only
// (internal/events).
//
// Every write is idempotent by construction:
//
//   - (order_id, event_id) is claimed exactly once via
//     saga_processed_events: a redelivered event returns
//     claimed=false and is skipped.
//   - Transition requires the expected current state: two
//     deliveries of the same event can never move the saga forward
//     twice.
//   - Terminal states (shipped, compensated_cancelled,
//     dead_letter) reject every further transition — a late
//     duplicate cannot resurrect a finished saga.
//
// Crashes between steps leave the row one step behind; the
// orchestrator's reconciler sweep resumes non-terminal sagas by the
// order_sagas_retry_idx index (next_retry_at), which is why
// RecordFailure stamps next_retry_at.
package saga

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Saga states (migration 0013's CHECK constraint — keep in sync).
const (
	StateStarted              = "started"               // order.created consumed; nothing verified yet
	StateReserved             = "reserved"              // reservation verified present
	StateConfirmed            = "confirmed"             // order transitioned created -> confirmed
	StateAwaitingBooking      = "awaiting_booking"      // confirmed; logistics booking pending (retry schedule)
	StateShipped              = "shipped"               // terminal: shipment booked, order shipped
	StateCompensatedCancelled = "compensated_cancelled" // terminal: reservation failed, order cancelled
	StateDeadLetter           = "dead_letter"           // terminal: booking retries exhausted, ops intervene
)

// terminalStates lists the states that reject all further transitions.
var terminalStates = map[string]bool{
	StateShipped:              true,
	StateCompensatedCancelled: true,
	StateDeadLetter:           true,
}

// ErrNotFound is returned when no saga exists for the order.
var ErrNotFound = errors.New("saga: no saga found for order")

// ErrStateMismatch is returned when Transition is called with an
// expected state that does not match the saga's current state —
// typically a duplicate event racing a completed step.
var ErrStateMismatch = errors.New("saga: current state does not match expected state")

// ErrTerminalState is returned when a transition is attempted on a
// saga that already reached shipped/compensated_cancelled/dead_letter.
var ErrTerminalState = errors.New("saga: saga is in a terminal state")

// Saga is one row of order_sagas.
type Saga struct {
	OrderID     pgtype.UUID
	State       string
	Attempts    int32
	LastError   *string
	NextRetryAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store persists saga state. The zero Store is not usable; use NewStore.
type Store struct {
	pool *db.Pool
}

// NewStore wraps the service pool. The pool stays owned by the
// caller (the API or the orchestrator), which closes it.
func NewStore(pool *db.Pool) *Store { return &Store{pool: pool} }

func scanSaga(row pgx.Row) (Saga, error) {
	var s Saga
	var lastErr *string
	var nextRetryAt, createdAt, updatedAt pgtype.Timestamptz
	if err := row.Scan(&s.OrderID, &s.State, &s.Attempts,
		&lastErr, &nextRetryAt, &createdAt, &updatedAt); err != nil {
		return Saga{}, err
	}
	s.LastError = lastErr
	if nextRetryAt.Valid {
		t := nextRetryAt.Time
		s.NextRetryAt = &t
	}
	s.CreatedAt = createdAt.Time
	s.UpdatedAt = updatedAt.Time
	return s, nil
}

const sagaColumns = "order_id, saga_state, attempts, last_error, next_retry_at, created_at, updated_at"

// Get returns the saga for an order, or ErrNotFound.
func (s *Store) Get(ctx context.Context, orderID pgtype.UUID) (Saga, error) {
	saga, err := scanSaga(s.pool.QueryRow(ctx,
		"SELECT "+sagaColumns+" FROM order_sagas WHERE order_id = $1", orderID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Saga{}, ErrNotFound
	}
	return saga, err
}

// Start registers the saga for an order and claims the triggering
// event, atomically. Returns isNew=true only when the saga row was
// freshly created AND the event was newly claimed — i.e. this call
// owns the first processing of the event.
//
// A redelivered order.created with the same event_id returns
// (saga, false, nil): idempotent, skip re-processing. A NEW event
// (e.g. order.status_changed) for an existing saga claims through
// ClaimEvent instead.
func (s *Store) Start(ctx context.Context, orderID pgtype.UUID, eventID string) (Saga, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Saga{}, false, fmt.Errorf("saga: begin start tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var created bool
	err = tx.QueryRow(ctx,
		`INSERT INTO order_sagas (order_id) VALUES ($1)
		 ON CONFLICT (order_id) DO NOTHING
		 RETURNING (xmax = 0)`,
		orderID).Scan(&created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING fired: the saga row already
			// exists, so this is a duplicate or out-of-order event.
			created = false
		} else {
			return Saga{}, false, fmt.Errorf("saga: insert saga row: %w", err)
		}
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO saga_processed_events (order_id, event_id) VALUES ($1, $2)
		 ON CONFLICT (order_id, event_id) DO NOTHING`,
		orderID, eventID)
	if err != nil {
		return Saga{}, false, fmt.Errorf("saga: claim event: %w", err)
	}
	claimed := tag.RowsAffected() == 1

	if err := tx.Commit(ctx); err != nil {
		return Saga{}, false, fmt.Errorf("saga: commit start tx: %w", err)
	}
	saga, err := s.Get(ctx, orderID)
	if err != nil {
		return Saga{}, false, err
	}
	return saga, created && claimed, nil
}

// ClaimEvent records a subsequent event for an existing saga
// (e.g. order.status_changed after the saga started). Returns
// claimed=false when the (order_id, event_id) pair was already
// processed — the caller must skip it. Returns ErrNotFound when no
// saga exists for the order.
func (s *Store) ClaimEvent(ctx context.Context, orderID pgtype.UUID, eventID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO saga_processed_events (order_id, event_id)
		 SELECT $1, $2
		 WHERE EXISTS (SELECT 1 FROM order_sagas WHERE order_id = $1)
		 ON CONFLICT (order_id, event_id) DO NOTHING`,
		orderID, eventID)
	if err != nil {
		return false, fmt.Errorf("saga: claim event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the pair already exists (duplicate redelivery) or
		// the saga row is missing. Distinguish so callers can react
		// properly.
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM saga_processed_events
			  WHERE order_id = $1 AND event_id = $2`, orderID, eventID).Scan(&n); err != nil {
			return false, fmt.Errorf("saga: check duplicate event: %w", err)
		}
		if n > 0 {
			return false, nil // duplicate redelivery — skip
		}
		return false, ErrNotFound
	}
	return true, nil
}

// Transition moves the saga from expectedState to nextState in one
// guarded write: the row updates only when the current state
// matches and is non-terminal. attempts increments so ops can see
// how many times the saga has been touched; updated_at bumps via
// the set_updated_at trigger.
//
// Zero rows affected means the saga is missing (ErrNotFound), is in
// a terminal state (ErrTerminalState), or the state already moved on
// (ErrStateMismatch) — the caller distinguishes via Get.
func (s *Store) Transition(ctx context.Context, orderID pgtype.UUID, expectedState, nextState string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE order_sagas
		    SET saga_state = $2, attempts = attempts + 1, last_error = NULL
		  WHERE order_id = $1
		    AND saga_state = $3
		    AND saga_state NOT IN ('shipped', 'compensated_cancelled', 'dead_letter')`,
		orderID, nextState, expectedState)
	if err != nil {
		return fmt.Errorf("saga: transition: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	saga, gerr := s.Get(ctx, orderID)
	if gerr != nil {
		return gerr // ErrNotFound
	}
	if terminalStates[saga.State] {
		return ErrTerminalState
	}
	return ErrStateMismatch
}

// RecordFailure marks the current step as failed with the error and
// schedules the next retry. The orchestrator then decides: retry the
// step (the reconciler sweep picks it up by next_retry_at) or move
// the saga to a failure exit. lastErr is stored verbatim for ops.
func (s *Store) RecordFailure(ctx context.Context, orderID pgtype.UUID, lastErr string, nextRetryAt time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE order_sagas
		    SET attempts = attempts + 1, last_error = $2, next_retry_at = $3
		  WHERE order_id = $1
		    AND saga_state NOT IN ('shipped', 'compensated_cancelled', 'dead_letter')`,
		orderID, lastErr, nextRetryAt)
	if err != nil {
		return fmt.Errorf("saga: record failure: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	saga, gerr := s.Get(ctx, orderID)
	if gerr != nil {
		return gerr
	}
	if terminalStates[saga.State] {
		return ErrTerminalState
	}
	return ErrNotFound // unreachable in practice; kept for exhaustiveness
}
