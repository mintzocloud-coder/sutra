# SPDX-License-Identifier: AGPL-3.0-or-later
# Logistics gateway (F5)

How Sutra books, tracks, and reconciles shipments — and why the real
courier integration lands after investors.

## Status: disabled by design

There is no logistics provider wired up. `internal/logistics.NewProvider`
accepts only `"stub"` (and `""`), and the stub refuses every call with a
typed `*ErrLogisticsDisabled` — it cannot book a pickup, move a parcel, or
report a fake delivery in any environment. Booking a real courier
(Delhivery / Shiprocket / a regional 3PL) is an operational decision that
lands after investors: signed agreement, live API credentials, and
seller pickup points registered. Until then the pipeline runs on COD
orders with **no shipment rows** — the `shipments` table and the event
contract below exist so F6's saga has a model to write to, not a fake
world to read from.

This is the same fail-closed philosophy as the payments seam
(`docs/payments.md`) and the D1 OTP seam (`docs/identity.md`).

## The seam (`internal/logistics.Provider`)

Three methods, all returning typed errors on the stub:

- `CreateShipment(ctx, ShipmentParams) (*ShipmentResult, error)` —
  books a pickup. A nil error means "the courier accepted the booking",
  exactly like `payments.Provider`'s nil-means-accepted contract.
  `ShipmentParams` carries `COD bool` + `CODAmountPaise int64` — Wave 1
  is COD-first, so the booking surface declares cash intent explicitly.
- `Track(ctx, trackingID) (*TrackingUpdate, error)` — partner's latest
  scan. Nil error means "the partner answered", not "the parcel is
  delivered" — callers read `Status`, never the error, for outcomes.
  `TrackingUpdate` carries the normalized `TrackingStatus` plus the
  partner's raw scan code (`StatusDetail`, debugging only) and
  `CODCollectedPaise` — what the rider actually collected at the door.
- `Cancel(ctx, shipmentID) (*CancelResult, error)` — pre-pickup
  cancellation; the real adapter rejects post-pickup cancels.

`TrackingStatus` is an 8-value lifecycle with a wire-stability test
pinning the exact strings, because the DB CHECK, the Go seam, and the
Elixir event validator must all agree:

| Status | Meaning |
|---|---|
| `created` | Booking accepted, awaiting pickup |
| `picked_up` | Courier has the parcel |
| `in_transit` | Moving between hubs |
| `out_for_delivery` | On the rider's route |
| `delivered` | Handed over (COD: cash collected) |
| `failed_delivery` | Rider could not deliver (door closed, refusal) |
| `cancelled` | Cancelled before pickup |
| `returned` | RTO — parcel came back to the seller |

Money is integer paise everywhere (same convention as
`internal/payments`). Service levels are `standard` (surface) and
`express` (air).

## The shipment tracking model (migration 0012)

`shipments` is the operational ledger for fulfilment, and the money
ledger for COD:

- `order_id → orders(id)` with `ON DELETE RESTRICT`: an order is a legal
  record, and deleting an order that has shipments must fail loudly.
- `provider`, `provider_shipment_id`, `tracking_number` — the courier's
  identifiers; `status` carries the 8-value lifecycle (DB CHECK identical
  to the Go wire values).
- **COD money**: `cod_expected_paise` (what should be collected — the
  order total for COD orders) and `cod_collected_paise` (what the rider
  reported at the door; `NULL` = not reported yet, never zero-filled —
  a missing report is not a zero collection).
- **Remittance**: a COD parcel has two money moments. (1) The door
  handover: rider collects cash → `cod_collected_paise`. (2) The
  remittance: the courier wires the collected cash to the seller /
  aggregator later → `remittance_status` `pending → remitted` (stamped in
  `remitted_at`; the `shipments_remitted_stamped` CHECK enforces the
  stamp). Shortfalls — collected < expected, a routine Indian-COD case
  (part payments, door refusals, rider mis-reporting) — are surfaced,
  not rounded: they sit as `pending` rows with a visible gap for ops to
  chase. Prepaid orders (Wave 2+) get `remittance_status = 'na'`.

Indexes: `shipments_order_idx`, `shipments_status_idx`, and a partial
`shipments_remittance_idx` on `remittance_status = 'pending'` — the
reconciliation query ("who owes us what") reads that index, not a scan.

## Realtime shipment events

The API publishes `shipment:update` to the chat's `POST /internal/events`
endpoint, which fans it out on the **buyer's** `user:<subject>` topic —
buyers get only their own orders' events (topic join auth, D5).

Payload:

```json
{
  "shipment_id": "s1", "order_id": "o1",
  "status": "out_for_delivery",
  "tracking_number": "DLV123456789",
  "cod_expected_paise": 99900,
  "cod_collected_paise": 99900,
  "updated_at": "2026-10-19T00:00:00Z"
}
```

`cod_collected_paise` is omitted until the courier reports. The chat
validates the envelope strictly (unknown statuses and negative money
→ 422 with the shared error envelope) but treats the fields as opaque —
it never interprets them.

**Producer**: the F6 saga orchestrator, which publishes *after the DB
write commits*, never before — an event always describes committed
state. The Go helper is `realtime.PublishShipmentUpdate`; the dedupe
key is `shipment:<shipment_id>:<status>:<updated-at-second>`, so a
retried saga step re-publishes the same key and clients dedupe on it.
Delivery is best-effort (same contract as `post:new` / `follower:new`,
`docs/realtime-events.md`): events are notification hints, Wave 2's
persistent inbox adds receipts.

The event contract is also documented in `docs/realtime-events.md`'s
event catalog.

## Post-investors onboarding checklist

- [ ] Sign the courier agreement (credit terms, COD remittance cycle —
  Delhivery/Shiprocket typically remit on a T+2/T+7 cycle; lock the
  schedule in writing, it drives the reconciliation SLA).
- [ ] Obtain live API credentials → `LOGISTICS_PROVIDER`,
  `LOGISTICS_API_KEY`, `LOGISTICS_API_SECRET`, `LOGISTICS_ENV` (never
  in the repo; never logged).
- [ ] Register seller pickup points / warehouses with the partner.
- [ ] Implement `Provider` against the partner API: shipment booking +
  label generation, tracking webhooks, pre-pickup cancellation.
- [ ] Register the provider in `NewProvider` under `LOGISTICS_PROVIDER`;
  keep `"stub"` valid for tests and dev.
- [ ] Wire the tracking webhook → update `shipments` → publish
  `shipment:update` via the F6 saga (or its webhook consumer).
- [ ] COD remittance reconciliation: pull the partner's settlement
  report, match against `shipments` pending rows, stamp
  `remittance_status = 'remitted'` + `remitted_at`.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `LOGISTICS_PROVIDER` | `stub` | Only `stub`/empty boots; anything else fails closed |
| `LOGISTICS_API_KEY` | — | Partner credential (post-investors; never logged) |
| `LOGISTICS_API_SECRET` | — | Partner credential (post-investors; never logged) |
| `LOGISTICS_ENV` | `sandbox` | `sandbox`/`production`; bad values refuse boot |

## Failure modes

| Situation | Behaviour |
|---|---|
| `LOGISTICS_PROVIDER=bogus` at startup | `invalid logistics config — refusing to start` before any DB touch |
| Stub `CreateShipment` called | `*ErrLogisticsDisabled` — never a fake booking |
| Webhook from partner (post-investors) fails auth | Reject, retry later; DB unchanged |
| Courier never reports COD collected | `cod_collected_paise` stays NULL; row stays `pending` — visible, not hidden |
| Realtime publish fails | Saga logs; the DB row is the truth, event is a hint |
| Order deleted with shipments | Fails (RESTRICT) — fulfilment history is immutable |

Out of scope for Wave 3: multi-parcel shipments, seller-arranged
shipping, rate shopping across couriers, NDR (non-delivery-report)
workflows, insurance claims. The table has the keys (order_id,
tracking_number) a Wave-4 extension needs.
