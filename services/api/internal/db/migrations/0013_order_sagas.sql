-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0013: order saga state + processed-event ledger (F6, part 2).
--
-- The saga orchestrator (internal/saga, cmd/saga-orchestrator) drives
-- fulfilment for every Wave-3 order: reserve-verify -> confirm -> ship,
-- with compensating actions on failure. Postgres is the system of
-- record; NATS JetStream is the notification channel. The orchestrator
-- reconciles from these tables when the event bus was down or the
-- worker crashed mid-step — a saga row is the only durable memory of
-- "how far did this order get".
--
-- SAGA LIFECYCLE (order_sagas.saga_state):
--   started          — order.created consumed (or reconciled); nothing
--                      verified yet.
--   reserved         — every order line's reserved stock verified
--                      present (the checkout tx already reserved it;
--                      this step proves it survived).
--   confirmed        — order transitioned created -> confirmed by the
--                      platform (the seller-confirm edge, taken by the
--                      saga acting as the platform, not a user).
--   awaiting_booking — confirmed, but the logistics provider has not
--                      accepted a shipment booking yet (the stub
--                      provider always refuses, so Wave-3 v1 parks
--                      every order here with a retry schedule).
--   shipped          — shipment booked, order transitioned
--                      confirmed -> shipped. Terminal.
--   compensated_cancelled — reservation verification failed: the
--                      order was cancelled and the reservation
--                      released. Terminal.
--   dead_letter      — booking retries exhausted. Terminal; ops
--                      intervene (retry by hand or wire the real
--                      provider, then reset the saga row).
--
-- Idempotency: saga_processed_events records every (order_id,
-- event_id) the orchestrator consumed. JetStream may redeliver (Nak /
-- worker restart / duplicate publish); the claim insert is the single
-- serialization point — the loser of a racing duplicate is a no-op.
--
-- Steps are idempotent by construction too: the order state machine
-- rejects illegal edges (confirming an already-confirmed order is
-- skipped, not an error), and the shipment insert is guarded by the
-- unique shipments(order_id) index below. Crashing between any two
-- steps leaves the saga row one step behind, and the reconciler picks
-- it up on the next sweep.
--
-- Safe to re-run: IF NOT EXISTS throughout.

CREATE TABLE IF NOT EXISTS order_sagas (
    order_id       uuid        PRIMARY KEY REFERENCES orders(id) ON DELETE RESTRICT,
    saga_state     text        NOT NULL DEFAULT 'started',
    attempts       int         NOT NULL DEFAULT 0,
    last_error     text,
    next_retry_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT order_sagas_state_valid CHECK (saga_state IN (
        'started', 'reserved', 'confirmed', 'awaiting_booking',
        'shipped', 'compensated_cancelled', 'dead_letter'
    )),
    CONSTRAINT order_sagas_attempts_nonnegative CHECK (attempts >= 0)
);

DROP TRIGGER IF EXISTS order_sagas_set_updated_at ON order_sagas;
CREATE TRIGGER order_sagas_set_updated_at
    BEFORE UPDATE ON order_sagas
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX IF NOT EXISTS order_sagas_retry_idx ON order_sagas(next_retry_at)
    WHERE saga_state = 'awaiting_booking';

-- Every event the orchestrator has consumed, keyed by (order_id,
-- event_id): the dedupe ledger. JetStream redelivers are the norm
-- (restarts, Naks, the 24h stream dedupe window expiring); this table
-- is what makes "exactly-once effects" true.
CREATE TABLE IF NOT EXISTS saga_processed_events (
    order_id     uuid        NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    event_id     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, event_id)
);

-- One shipment per order in v1 (multi-parcel splitting is a Wave-4
-- concern). The saga's booking step inserts guarded by this index, so
-- a redelivered order.created can never create two shipments.
CREATE UNIQUE INDEX IF NOT EXISTS shipments_order_unique ON shipments(order_id);
