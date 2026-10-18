<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Sutra Commerce — Wave 3 (Phase F)

Sutra's commerce core: a 0%-commission marketplace (the Meesho model),
built in Go on the Phase A–E foundation. COD-first in v1; online
payments and third-party logistics are explicit post-investors seams.

## The pieces

| Piece | Plan | Where it lives |
|---|---|---|
| Seller onboarding + catalog | F1 | `internal/catalog`, migration 0008 |
| Products, variants, inventory | F1b | `internal/catalog`, migration 0008 |
| Product search (Meilisearch CE) | F2 | `internal/search`, migrations 0008–0009, `docs/search.md` |
| Cart + COD checkout + orders | F3 | `internal/cart`, `internal/orders`, migrations 0010–0011 |
| Payment-aggregator seam | F4 | `internal/payments` (fail-closed stub), `docs/payments.md` |
| Logistics-gateway seam | F5 | `internal/logistics` (fail-closed stub), shipments migration 0012, `docs/logistics.md` |
| Order events (NATS JetStream) | F6.1 | `internal/events` (`sutra` stream, `sutra.orders.>` subjects) |
| Saga orchestrator (state store + engine + consumer) | F6.2 | `internal/saga`, `internal/events/consumer.go`, migration 0013 |
| Commerce rate budget | F6.3 | `internal/middleware/commerce_ratelimit.go`, `COMMERCE_LIMIT_RPS`/`COMMERCE_LIMIT_BURST` |

## Catalog (F1/F1b)

- One seller account per user; first-come `store_slug`s (`[a-z0-9-]{3,30}`).
- Money is integer paise, price on the variant; `mrp >= price` enforced.
- **0% commission v1**: `commission_rate_bps` defaults to 0; the
  payments split design reconciles it via `SplitParams.CommissionBps`
  (see `docs/payments.md`).
- Inventory: `quantity` / `reserved`, `reserved <= quantity` CHECK.
  `AdjustInventory` is one atomic guarded UPDATE that refuses
  negative stock (409 `insufficient_stock`). `reserved` is owned by the
  order saga — nothing else touches it.

## Search (F2)

Meilisearch CE (MIT, pinned) self-hosted; stdlib-only Go client.
Active products index on write (detached, best-effort, never delays
the 201); drafts and archived listings leave the index. Variant
writes rebuild the parent product document (cheapest active variant
price). `GET /v1/search` proxies Meilisearch with numeric
`(updated_at, sort_seq)` keyset cursors — never OFFSET. Full contract
in `docs/search.md`.

## Cart + orders (F3)

- Cart: one row per user; accumulating upserts; stock re-verified in
  the same transaction as the add; lines keep live prices.
- Checkout: reads buyable lines, re-verifies stock under row locks,
  reserves inventory, snapshots prices immutably into `order_items`
  (legal history — later reprices never rewrite an order), splits
  multi-seller carts into one order per seller, clears the cart.
  Empty cart → 422; stock overflow → 409 with the cart intact.
- State machine: `created → confirmed → shipped → delivered`, with
  `cancelled` (buyer/seller from created, seller from confirmed) and
  `returned` (buyer/seller from delivered). Cancel/return releases the
  reservation; deliver converts it into a sale. All transitions are
  row-locked; unknown/illegal edges → 422.

## Gated seams (F4/F5)

- **Payments**: `PAYMENTS_PROVIDER` is `"stub"` until the Cashfree
  onboarding completes **post-investors** (founder order 2026-10-17).
  Every stub operation is a typed `ErrPaymentsDisabled` — never a
  charge, never a faked success. UPI Intent-first design, Easy Split
  vendor payouts, and the post-investors checklist live in
  `docs/payments.md`.
- **Logistics**: `LOGISTICS_PROVIDER` is `"stub"` until a partner is
  onboarded post-investors. Same fail-closed contract; the
  `shipments` table (migration 0012) already carries COD remittance
  fields (`cod_expected_paise`, `cod_collected_paise`,
  `remittance_status` pending/remitted/na). See `docs/logistics.md`.

## Order events + saga (F6)

- Checkout/Transition publish `order.created` /
  `order.status_changed` onto the `sutra` JetStream stream
  (`sutra.orders.created`, `sutra.orders.status_changed`) with the
  event id as the JetStream message id (24h dedupe window — retries
  never double-deliver). Events are notifications, not the system of
  record: the write commits first, publishing is detached and
  best-effort.
- The saga orchestrator (`internal/saga`) drives
  `started → reserved → confirmed`: reservation re-verification,
  COD auto-confirm, compensation on broken reservations
  (`compensated_cancelled`), transient-failure backoff
  (`RecordFailure`, 30s-doubling/30m-capped), idempotent on
  `(order_id, event_id)` via the `saga_processed_events` claim.
- **Honest v1 cut**: `confirmed → awaiting_booking → shipped` is NOT
  implemented — booking needs a shipping address and the orders model
  carries none (Wave 4). The engine never fakes a shipment.
- Durable consumer (`internal/events/consumer.go`): pull consumer
  `saga-orchestrator`, AckExplicit, AckWait 30s, MaxDeliver 10;
  poison messages are acked + logged, handler errors Nak for
  redelivery.

## Rate budgets (F6.3)

- The global per-IP limiter (`RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`,
  default 100/200) is the outer ceiling.
- Commerce paths stack a tighter budget on top
  (`COMMERCE_LIMIT_RPS`/`COMMERCE_LIMIT_BURST`, default 30/60;
  `<=0` disables): `/v1/sellers`, `/v1/products`, `/v1/variants`,
  `/v1/categories`, `/v1/search`, `/v1/cart`, `/v1/orders`.
  Rejections are 429 `commerce_rate_limited` with Retry-After
  (the global budget's is `rate_limited`).
- DB-backed velocity caps remain the semantic abuse guards:
  `POST_VELOCITY_PER_HOUR` (30) for listings,
  `FOLLOW_VELOCITY_PER_HOUR` (200) for follows.

## Env reference (commerce)

| Var | Default | Notes |
|---|---|---|
| `COMMERCE_LIMIT_RPS` | `30` | Commerce-surface sustained rate per IP |
| `COMMERCE_LIMIT_BURST` | `60` | Commerce-surface burst per IP |
| `SEARCH_URL` / `SEARCH_MASTER_KEY` | `http://meilisearch:7700` / empty | Empty key disables indexing (dev); bad URL + key refuses boot |
| `NATS_URL` | empty | `nats://nats:4222` in compose; empty disables the event bus (dev) |
| `PAYMENTS_PROVIDER` | `stub` | Fail-closed; only `stub` boots pre-investors |
| `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY` / `CASHFREE_ENV` | unset | Loaded, never logged, never committed |
| `LOGISTICS_PROVIDER` | `stub` | Fail-closed; only `stub` boots pre-investors |
| `LOGISTICS_API_KEY` / `LOGISTICS_SECRET` / `LOGISTICS_ENV` | unset | Loaded, never logged, never committed |

Smoke coverage: `scripts/smoke.sh` carries the full commerce
round-trips (F1b catalog, F3 cart+order, F2 search, F6 event-bus +
commerce-budget 429s).
