<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Payments (F4)

Online payment processing for Sutra commerce, designed around
**Cashfree** (the post-investors aggregator) and India-first **UPI
Intent-first** checkout. Currency is pinned to INR; money is integer
paise everywhere (same convention as `internal/catalog`).

## Status: disabled by design

Until the Cashfree onboarding completes post-investors, the only
provider that boots is the **fail-closed stub** (`internal/payments`):

- `PAYMENTS_PROVIDER` accepts only `"stub"` (or empty, which maps to
  it). `"cashfree"` — or any other value — makes the API **refuse to
  boot** (`os.Exit(1)`), so a misconfigured provider can never
  silently take no payments.
- Every method on the stub (`CreateOrder`, `VerifyWebhook`,
  `SplitTransfer`, `Payout`) returns a typed
  `*payments.ErrPaymentsDisabled` — an explicit error, never a faked
  success and never a silent no-op.
- A bad `CASHFREE_ENV` (anything but `""`/`sandbox`/`prod`) is a
  boot-time error. Bad config shouts; it is never silently ignored.
- `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY` / `CASHFREE_ENV` are
  loaded, never logged (not even the App ID), never committed. While
  the stub is wired and any of them are set, the API logs a loud
  WARN that they are **ignored** — keys sitting in env are not
  "almost wired"; they are dead until a real provider exists.
- Compose pins `PAYMENTS_PROVIDER: "stub"` and passes the three
  `CASHFREE_*` values through empty by default.

The F6 order saga will call this seam. While the stub is wired, any
saga step that touches money fails with `ErrPaymentsDisabled` and the
order stays unpaid — an honest error to the caller, never a
"pretended paid" state.

## The seam (`internal/payments.Provider`)

Four methods, concurrency-safe implementations required, no full
credential values in logs:

| Method | Request | Result | Notes |
|---|---|---|---|
| `CreateOrder` | `OrderParams` (OrderRef, AmountPaise, Currency=`INR`, buyer identity, `UPIIntent` flag) | `OrderResult` (ProviderOrderID, SessionID, PaymentLink, UPIIntentURL) | `OrderRef` is our internal order uuid — the idempotency key at the aggregator; retried checkouts reuse it, so a retried checkout never creates a second aggregator order |
| `VerifyWebhook` | raw body + signature | `WebhookEvent` (`paid` / `failed`, PaidAt capture timestamp) | Implementations MUST reject bad signatures — a nil error means "the aggregator said this" |
| `SplitTransfer` | `SplitParams` (OrderRef, VendorAccount, AmountPaise, CommissionBps) | `SplitResult` | Moves the vendor's share of one paid order via the aggregator's split product |
| `Payout` | `PayoutParams` (BeneficiaryID, AmountPaise, Mode=`upi`/`imps`/`neft`) | `PayoutResult` | Settles accumulated vendor earnings to their bank/UPI |

## UPI Intent-first checkout

UPI is the buyer's default rail — no card entry, no OTP dance, money
leaves the buyer's bank app directly:

1. Buyer checks out (F3 order created, state `unpaid`).
2. F6 saga calls `CreateOrder` with `UPIIntent: true` and the
   buyer's E.164 phone (required for the Intent flow).
3. Cashfree returns a payment session; the response carries a
   `upi://` deep link (`OrderResult.UPIIntentURL`) — the app opens
   it, the buyer authorizes inside their UPI app.
4. Cashfree POSTs a webhook → `VerifyWebhook` authenticates it →
   order flips `unpaid` → `paid` → split transfer fires (below).
5. Fallback: `PaymentLink` is the hosted web payment page for
   desktop buyers or future international cards (post-launch rail).

Why Intent-first: it skips the aggregator's hosted page entirely on
mobile, the failure modes are the buyer's bank app's problem (not
ours), and it is the cheapest rail available — the marketplace's
0%-commission model can't fund expensive card processing.

## Easy Split vendor design (Cashfree Easy Split)

The marketplace model is **0% commission in v1** (migration 0008):
the vendor receives the *full* order total; the platform takes
nothing. Cashfree Easy Split is the mechanism:

- Each seller becomes a **beneficiary** on our Cashfree Easy Split
  account at vendor onboarding (seller submits bank/UPI details in
  the onboarding flow; we create the beneficiary via the Easy Split
  API post-investors; the beneficiary id is stored on the seller
  record — never created in a half-wired state).
- On every `paid` event the F6 saga calls `SplitTransfer` with the
  full order amount and `CommissionBps: 0`. The `CommissionBps`
  field exists for **reconciliation**: the split ledger records
  what the platform take *was* on each transfer, so when pricing
  changes (commission > 0 bps), the history reads correctly —
  no schema migration, no backfill.
- Easy Split charges a per-transfer split fee (≈0.1% — **confirm
  against Cashfree's live pricing sheet during onboarding**; this
  doc does not set the number). In v1 the platform absorbs it as
  marketplace cost; the onboarding checklist must record the final
  accounting decision (absorb vs pass to vendor) explicitly.
- **COD orders never touch this path**: COD is cash-on-delivery
  handled by the logistics partner; the split product only moves
  prepaid online money. COD remittance tracking is F5's territory
  (`docs/logistics.md`).

## Webhook + signature verification (design for the real adapter)

Cashfree signs its callbacks; `VerifyWebhook` must:

- Verify the HMAC signature against `CASHFREE_SECRET_KEY`
  (timestamp-scoped — reject stale timestamps to kill replays).
- Normalize to `WebhookEvent{paid, failed}` with the aggregator's
  capture timestamp (`PaidAt`, zero on failure).
- Be **idempotent by OrderRef**: a `paid` callback for an already-paid
  order returns 200 to the aggregator and changes nothing — webhook
  retries must never double-credit or double-split.
- Handle out-of-order events: a `failed` arriving after `paid`
  (late cancellation) must not un-pay a split order; it becomes a
  support-visible event, and any money movement goes through the
  refund path below.

Until wired, `VerifyWebhook` returns `ErrPaymentsDisabled` — it never
authenticates a callback. A webhook endpoint that accepts unverified
callbacks is the one payment bug that loses real money silently, so
this method stays fail-closed until the real HMAC check ships.

## Refund path (design)

Refunds are money moving *backwards* — same care as payouts:

- A refund is initiated against the original provider payment
  (`ProviderOrderID`) via Cashfree's refund API; the F6 state machine
  carries the order `paid → refund_requested → refunded` (F6 owns the
  states; this doc owns the money semantics).
- Vendor clawback: the refunded amount is deducted from the vendor's
  pending payout first; only if the pending balance is insufficient
  does a split reversal/debit fire — never a silent platform loss.
- Refunds in v1 are **support-triggered** (a support tool calls the
  seam), never automatic on cancellation. Automatic refunds need
  policy sign-off the company doesn't have yet.
- Fail-closed today: a refund call on the stub is a typed error, so
  no F6 code path can accidentally "complete" a refund against
  nothing.

## Post-investors onboarding checklist

Nothing below is wired today; this is the checklist that turns the
stub into the Cashfree adapter. Do not mark any item done from a
code read — each needs a live artefact.

- [ ] Signed Cashfree agreement + merchant account (legal entity
  must exist — post-incorporation).
- [ ] Bank settlement account linked to the Cashfree merchant
  account; settlement T+X confirmed in writing.
- [ ] API credentials issued: App ID + Secret Key for **sandbox**
  and **prod** separately; stored in the secret store, never in
  `.env` on a shared machine.
- [ ] Easy Split product activated on the merchant account; split
  fee (currently ≈0.1%/transfer) confirmed in writing from the
  current pricing sheet.
- [ ] Vendor beneficiary onboarding flow built and tested: seller
  submits bank/UPI details → beneficiary created via Easy Split API
  → beneficiary id stored on the seller record; failure states
  (rejected IFSC, duplicate beneficiary) handled.
- [ ] Webhook endpoint registered with Cashfree; webhook secret
  stored; `VerifyWebhook` HMAC + timestamp checks live-tested
  against sandbox callbacks (including replayed and tampered ones).
- [ ] Sandbox end-to-end: UPI Intent test payment → `paid` webhook
  → split to a test beneficiary → refund → payout, all verified on
  the Cashfree dashboard.
- [ ] UPI Intent deep-link tested on real devices (Android intent
  resolution + iOS universal-link fallback to the hosted page).
- [ ] Accounting decision recorded: who absorbs the Easy Split
  per-transfer fee (platform in v1).
- [ ] `PAYMENTS_PROVIDER=cashfree` flipped in staging first, then
  prod; compose pin updated; the fail-closed WARN disappearing from
  boot logs is the observable "payments are live" signal.

## Environment

| Var | Default | Purpose |
|---|---|---|
| `PAYMENTS_PROVIDER` | `stub` | Only `stub` boots until the post-investors Cashfree onboarding; anything else refuses to boot (fail-closed) |
| `CASHFREE_APP_ID` | *(unset)* | Cashfree App ID; loaded, never logged, ignored by the stub (WARN when set) |
| `CASHFREE_SECRET_KEY` | *(unset)* | Cashfree Secret Key; same handling as the App ID |
| `CASHFREE_ENV` | *(unset)* | `sandbox` or `prod`; malformed values refuse boot |

## Failure modes

| Situation | Behavior |
|---|---|
| `PAYMENTS_PROVIDER` unknown | API refuses to boot (`os.Exit(1)`) — loud error, no half-state |
| `CASHFREE_ENV` malformed | API refuses to boot before touching the DB |
| Any stub method called | Typed `*payments.ErrPaymentsDisabled` — explicit, never a fake |
| `CASHFREE_*` set but stub wired | Boot succeeds with a loud WARN: keys are ignored, payments still disabled |
| Webhook with bad signature (real adapter) | Rejected — never normalized into an event |
| Split/payout transport failure (real adapter) | Saga retries with backoff (F6); the transfer is idempotent on `OrderRef`, so retries never double-move money |
