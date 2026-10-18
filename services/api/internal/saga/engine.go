// SPDX-License-Identifier: AGPL-3.0-or-later
// Package saga — orchestrator engine (F6 part 2b): the durable
// JetStream consumer's event handler, driving
// started -> reserved -> confirmed with compensating actions.
//
// STEP MODEL (v1):
//
//   - started -> reserved: verify the checkout reservation is still
//     intact (every line's reserved stock still covers the ordered
//     quantity). Checkout reserved inside its own transaction; this
//     re-check catches admin inventory edits or data corruption that
//     landed between checkout and the saga.
//   - reserved -> confirmed: COD auto-confirm. COD has no payment to
//     wait for, so the saga confirms the order itself; a concurrent
//     seller confirm is a no-op (re-read, move on).
//   - compensation: a broken reservation releases whatever is left
//     and cancels the order (orders.Transition's cancel side effect
//     does the release), then parks the saga in compensated_cancelled.
//   - external death: an order.status_changed with cancelled/returned
//     (buyer/seller acted before the saga started, or out-of-order
//     events) parks the saga in compensated_cancelled — the order's
//     own transition already released the reservation.
//
// NOT in v1: confirmed -> awaiting_booking -> shipped. Booking needs
// the logistics provider AND a shipping address, and the orders model
// carries no address yet (Wave 4). The engine deliberately has no
// booking step: it will not fake a shipment. The dead_letter exit and
// the reconciler sweep (by next_retry_at) land with cmd/saga-worker
// (F6 part 3).
//
// IDEMPOTENCY: every path is safe in (order_id, event_id). Start
// claims the triggering event (redelivery → isNew=false → skip);
// ClaimEvent does the same for status_changed; state-guarded
// transitions make double-applied steps no-ops; terminal states
// reject everything. Transient failures RecordFailure with an
// exponential-backoff next_retry_at and return the error so the
// consumer redelivers the message.
package saga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/events"
	"github.com/mintzocloud-coder/sutra/services/api/internal/orders"
)

// SagaStore is the subset of *Store the engine drives. *Store
// satisfies it; tests use fakes.
type SagaStore interface {
	Start(ctx context.Context, orderID pgtype.UUID, eventID string) (Saga, bool, error)
	ClaimEvent(ctx context.Context, orderID pgtype.UUID, eventID string) (bool, error)
	Transition(ctx context.Context, orderID pgtype.UUID, expectedState, nextState string) error
	RecordFailure(ctx context.Context, orderID pgtype.UUID, lastErr string, nextRetryAt time.Time) error
	Get(ctx context.Context, orderID pgtype.UUID) (Saga, error)
}

// OrderOps is the slice of the orders service the engine needs:
// system-role reads and transitions (never the HTTP-facing ones).
type OrderOps interface {
	GetSystem(ctx context.Context, orderID pgtype.UUID) (*orders.Order, error)
	TransitionSystem(ctx context.Context, orderID pgtype.UUID, to string) (*orders.Order, error)
}

// InventoryOps is the slice of the catalog service the engine needs:
// reservation-intact checks (public storefront reads).
type InventoryOps interface {
	GetInventory(ctx context.Context, variantID pgtype.UUID) (*catalog.Inventory, error)
}

// Retry schedule for transient step failures: 30s doubling, capped
// at 30m. The reconciler sweep (F6 part 3) re-drives sagas by
// next_retry_at; until then, JetStream redelivery (AckWait) is the
// retry loop.
const (
	retryBase = 30 * time.Second
	retryCap  = 30 * time.Minute
)

// Engine drives the saga step machine for one order event at a time.
// It is safe for concurrent use as long as the underlying services
// are (they are); v1 runs a single consumer, so steps are naturally
// serialized per durable.
type Engine struct {
	store  SagaStore
	orders OrderOps
	inv    InventoryOps
	log    *slog.Logger
}

// NewEngine wires the engine. The zero Engine is not usable.
func NewEngine(store SagaStore, orderOps OrderOps, invOps InventoryOps, log *slog.Logger) *Engine {
	return &Engine{store: store, orders: orderOps, inv: invOps, log: log}
}

// HandleEvent implements events.EventHandler: it routes one order
// event to the saga step machine. A nil return acknowledges the
// message; a non-nil return redelivers it per the consumer's
// AckWait/MaxDeliver policy.
func (e *Engine) HandleEvent(ctx context.Context, ev events.OrderEvent) error {
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("saga: invalid event envelope: %w", err)
	}
	orderID, err := parseUUID(ev.OrderID)
	if err != nil {
		return fmt.Errorf("saga: bad order_id %q: %w", ev.OrderID, err)
	}
	switch ev.Event {
	case events.OrderCreated:
		return e.handleCreated(ctx, orderID, ev.EventID)
	case events.OrderStatusChanged:
		return e.handleStatusChanged(ctx, orderID, ev.EventID, ev.Status)
	default:
		e.log.Warn("saga: unknown event, skipping",
			"event", ev.Event, "order_id", ev.OrderID)
		return nil
	}
}

func parseUUID(id string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(id); err != nil || !u.Valid {
		return pgtype.UUID{}, errors.New("not a valid uuid")
	}
	return u, nil
}

// handleCreated drives started -> reserved -> confirmed for a fresh
// order.created event.
func (e *Engine) handleCreated(ctx context.Context, orderID pgtype.UUID, eventID string) error {
	_, isNew, err := e.store.Start(ctx, orderID, eventID)
	if err != nil {
		return fmt.Errorf("saga: start: %w", err)
	}
	if !isNew {
		e.log.Debug("saga: duplicate order.created, skipping",
			"order_id", orderID.String())
		return nil
	}

	ord, err := e.orders.GetSystem(ctx, orderID)
	if err != nil {
		return e.fail(ctx, orderID, "load", err)
	}

	// The order may have died before the saga started (out-of-order
	// status_changed, or a manual cancel racing checkout): a dead
	// order needs no saga. The stock side effects already ran inside
	// orders.Transition, so this is pure bookkeeping.
	switch ord.Status {
	case orders.StatusCancelled, orders.StatusReturned:
		e.log.Info("saga: order already dead, parking saga",
			"order_id", orderID.String(), "status", ord.Status)
		return e.advance(ctx, orderID, StateStarted, StateCompensatedCancelled)
	case orders.StatusCreated:
		// normal path below
	default:
		// confirmed/shipped/delivered: the seller raced ahead of the
		// saga. The steps below are idempotent, so the normal path
		// still applies (verify reservations, confirm is a no-op).
	}

	// STEP 1 — reserve: verify the checkout reservation is intact.
	intact, err := e.reservationsIntact(ctx, ord)
	if err != nil {
		return e.fail(ctx, orderID, "verify-reservations", err)
	}
	if !intact {
		// COMPENSATE: release whatever is left and cancel the order.
		// orders.Transition's cancel side effect does the release, so
		// one system transition is the whole compensation.
		if _, err := e.orders.TransitionSystem(ctx, orderID, orders.StatusCancelled); err != nil {
			return e.fail(ctx, orderID, "compensate-cancel", err)
		}
		e.log.Warn("saga: reservation broken, order cancelled (compensation)",
			"order_id", orderID.String())
		return e.advance(ctx, orderID, StateStarted, StateCompensatedCancelled)
	}
	if err := e.advance(ctx, orderID, StateStarted, StateReserved); err != nil {
		return err
	}

	// STEP 2 — confirm: COD auto-confirm (no payment to wait for).
	if ord.Status == orders.StatusCreated {
		if _, err := e.orders.TransitionSystem(ctx, orderID, orders.StatusConfirmed); err != nil {
			if errors.Is(err, orders.ErrInvalidTransition) {
				// The order moved concurrently (seller confirmed or
				// cancelled between our read and the transition);
				// re-read to see where it landed.
				ord, rerr := e.orders.GetSystem(ctx, orderID)
				if rerr != nil {
					return e.fail(ctx, orderID, "confirm-reread", rerr)
				}
				if ord.Status != orders.StatusConfirmed {
					return e.fail(ctx, orderID, "confirm",
						fmt.Errorf("order left created without confirming (status %q)", ord.Status))
				}
				// else: seller confirmed first — the no-op path.
			} else {
				return e.fail(ctx, orderID, "confirm", err)
			}
		}
	}
	return e.advance(ctx, orderID, StateReserved, StateConfirmed)
}

// handleStatusChanged reacts to order.status_changed events. The saga
// only cares when the order died by an external actor: a cancelled or
// returned order must not keep a retry schedule alive. Every other
// status is informational in v1 (the saga drives created→confirmed
// itself; booking/shipped tracking lands with the address model).
func (e *Engine) handleStatusChanged(ctx context.Context, orderID pgtype.UUID, eventID, status string) error {
	claimed, err := e.store.ClaimEvent(ctx, orderID, eventID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No saga for this order: a pre-saga order, or the
			// order.created hasn't arrived yet (cross-subject
			// out-of-order). Nothing to do either way.
			return nil
		}
		return fmt.Errorf("saga: claim event: %w", err)
	}
	if !claimed {
		e.log.Debug("saga: duplicate status_changed, skipping",
			"order_id", orderID.String())
		return nil
	}
	switch status {
	case orders.StatusCancelled, orders.StatusReturned:
		saga, err := e.store.Get(ctx, orderID)
		if err != nil {
			return fmt.Errorf("saga: get for external cancel: %w", err)
		}
		// The order's own transition already released the
		// reservation; park the saga in its failure exit.
		e.log.Info("saga: order cancelled externally, parking saga",
			"order_id", orderID.String(), "saga_state", saga.State)
		return e.advance(ctx, orderID, saga.State, StateCompensatedCancelled)
	default:
		return nil
	}
}

// reservationsIntact verifies the checkout reservation is still
// present: every line's reserved stock must still cover the ordered
// quantity. A missing inventory row is broken, not transient — the
// variant's stock record is gone, so the reservation went with it
// (→ compensate, not retry).
func (e *Engine) reservationsIntact(ctx context.Context, ord *orders.Order) (bool, error) {
	for _, it := range ord.Items {
		variantID, err := parseUUID(it.VariantID)
		if err != nil {
			return false, fmt.Errorf("bad variant_id %q: %w", it.VariantID, err)
		}
		inv, err := e.inv.GetInventory(ctx, variantID)
		if err != nil {
			if errors.Is(err, catalog.ErrVariantNotFound) {
				e.log.Warn("saga: inventory row missing for order line",
					"order_id", ord.ID, "variant_id", it.VariantID)
				return false, nil
			}
			return false, fmt.Errorf("get inventory %s: %w", it.VariantID, err)
		}
		if inv.Reserved < it.Quantity || inv.Quantity < 0 {
			e.log.Warn("saga: reservation broken",
				"order_id", ord.ID, "variant_id", it.VariantID,
				"reserved", inv.Reserved, "needed", it.Quantity,
				"quantity", inv.Quantity)
			return false, nil
		}
	}
	return true, nil
}

// advance moves the saga forward one step, swallowing the two benign
// races: the step already applied (ErrStateMismatch) or the saga
// already finished by a racing event (ErrTerminalState).
func (e *Engine) advance(ctx context.Context, orderID pgtype.UUID, from, to string) error {
	err := e.store.Transition(ctx, orderID, from, to)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrStateMismatch), errors.Is(err, ErrTerminalState):
		e.log.Debug("saga: step already applied, skipping",
			"order_id", orderID.String(), "from", from, "to", to)
		return nil
	default:
		return fmt.Errorf("saga: transition %s→%s: %w", from, to, err)
	}
}

// fail records a transient step failure with an exponential-backoff
// retry stamp and returns the error so the consumer redelivers the
// message. The attempts read is best-effort: a failed Get must not
// mask the real error.
func (e *Engine) fail(ctx context.Context, orderID pgtype.UUID, step string, err error) error {
	backoff := retryBase
	if saga, gerr := e.store.Get(ctx, orderID); gerr == nil {
		backoff = backoffFor(saga.Attempts)
	}
	if rerr := e.store.RecordFailure(ctx, orderID, step+": "+err.Error(), time.Now().Add(backoff)); rerr != nil {
		e.log.Error("saga: record failure failed",
			"order_id", orderID.String(), "step", step, "error", rerr)
	}
	return fmt.Errorf("saga: step %s: %w", step, err)
}

// backoffFor doubles the base per attempt, capped. attempts<0 is
// treated as 0 (defensive; the store never produces it).
func backoffFor(attempts int32) time.Duration {
	d := retryBase
	for i := int32(0); i < attempts && d < retryCap; i++ {
		d *= 2
	}
	if d > retryCap {
		d = retryCap
	}
	return d
}
