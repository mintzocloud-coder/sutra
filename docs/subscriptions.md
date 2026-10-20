<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Subscriptions (H4)

Creator-membership billing for Sutra: a creator defines paid membership
plans (tiers); members subscribe; billing rides **Cashfree
Subscriptions** with **UPI AutoPay** mandates — India's recurring-
payments rail (RBI e-mandate, no card entry, money leaves the member's
UPI app directly). This is the 70/30 income-stream engine from the
locked creator economics (2026-10-17): **the creator keeps 70%, the
platform takes 30%** (membership platform cut), every billing event,
reconciled on-ledger.

## Status: disabled by design

Until the Cashfree Subscriptions onboarding completes post-investors,
the only provider that boots is the **fail-closed stub**
(`internal/subscriptions`):

- `SUBSCRIPTIONS_PROVIDER` accepts only `"stub"` (or empty, which maps
  to it). `"cashfree"` — or any other value — makes the API **refuse
  to boot** (`os.Exit(1)`), so a misconfigured provider can never
  silently bill nothing (or bill through an unwired rail).
- Every method on the stub (`CreateSubscription`, `CancelSubscription`,
  `VerifyWebhook`) returns a typed `*subscriptions.ErrSubscriptionsDisabled`
  — an explicit error, never a faked mandate and never a silent no-op.
- Cashfree credentials are shared with the payments seam (`CASHFREE_*`
  env): loaded, never logged, never committed. While the stub is wired
  and any of them are set, the API logs a loud WARN that they are
  **ignored**.
- Compose pins `SUBSCRIPTIONS_PROVIDER: "stub"` and passes the shared
  `CASHFREE_*` values through empty by default.

## Data model (migration 0020)

| Table | Purpose |
|---|---|
| `subscription_plans` | Creator membership tiers: `creator_id`, `name` (≤80 chars), `description` (≤500), `price_paise` (> 0), `billing_period` (`monthly`/`yearly`), `currency` (pinned `INR`), `is_active`. Plans are soft-deactivated (`is_active=false`), never deleted while subscriptions reference them. |
| `subscriptions` | One row per member–plan billing relationship. Status lifecycle: `pending → active → cancelled`, with `past_due` as the recoverable failure state. `provider_ref` is the aggregator's subscription id — NULL until wired (NULL = no mandate exists anywhere; nothing can be charged). The partial unique index `(subscriber_id, plan_id) WHERE status IN ('active','past_due')` guarantees a member can never hold two live subscriptions on one plan (no double-billing surface). |
| `membership_events` | **Append-only billing ledger**: one row per billing outcome (`billing_succeeded`, `billing_failed`, `cancelled`) carrying `amount_paise` plus the 70/30 split (`creator_share_paise = floor(amount*70/100)`, `platform_share_paise` = remainder — the CHECK guarantees they sum exactly). `billing_succeeded` rows REQUIRE a `provider_ref` (no success without an aggregator receipt). Ledger-only until the Cashfree onboarding — no money moves today. |
| `creator_earnings` | Per-creator accrual target: `pending_paise` grows with each `billing_succeeded` event (same transaction as the ledger row); `paid_out_paise` moves only via `payments.Provider.Payout` once the aggregator is live (F4). While disabled, pending only grows — an honest, auditable record of what the platform owes, not a transfer. |

## The seam (`internal/subscriptions.Provider`)

Three methods, concurrency-safe implementations required, no full
credential values in logs:

| Method | Request | Result | Notes |
|---|---|---|---|
| `CreateSubscription` | `CreateParams` (MemberID, PlanID, AmountPaise, Currency=`INR`, Period, customer identity; E.164 phone required for the mandate flow) | `CreateResult` (ProviderSubID, Status, AuthURL) | `AuthURL` is the UPI AutoPay authorization session the member must complete — without it the mandate does not exist |
| `CancelSubscription` | providerSubID, atPeriodEnd | nil | `atPeriodEnd=true` keeps the member's access until `current_period_end`; the row flips to `cancelled` then |
| `VerifyWebhook` | raw body + signature | `WebhookEvent` (`billing_succeeded` / `billing_failed` / `cancelled`, AmountPaise, PaidAt) | Implementations MUST reject bad signatures — a nil error means "the aggregator said this" |

The pure domain layer (same package, fully unit-tested):

- `ValidatePlanInput` — name/description/price/period/currency checks,
  mirroring the migration CHECKs (rune-counted, emoji-safe).
- `SplitShares` — the 70/30 split math: `creator = floor(amount*70/100)`,
  `platform = amount - creator`. Two independent roundings would drift;
  floor-then-remainder guarantees `creator+platform == amount` exactly.
- `CanTransition` — the status matrix: `pending` leaves only via a
  verified aggregator event, `cancelled` is terminal (resurrection needs
  a new row, so billing history is never rewritten).
- `EntitlementsFor` — the entitlement model (below).

## Entitlement model

An ACTIVE subscription grants the member exactly one entitlement:

```
member_of:<creator_id>
```

Membership gating (exclusive posts, paid groups, live tiers, member
perks) matches on this string — **never on provider state** — so a
fail-closed subscription can never grant access: only a verified
`billing_succeeded` webhook event promotes a row to `active`, and the
stub can never emit one. The pending state confers nothing.

## The billing loop (design for the real adapter)

1. Member picks a plan → `POST /v1/subscriptions` (H4 part 2):
   `subscriptions` row created as `pending`; `CreateSubscription`
   called → member completes UPI AutoPay authorization via `AuthURL`.
2. Cashfree POSTs a webhook → `VerifyWebhook` authenticates (HMAC +
   timestamp, replay-rejected) → normalized `WebhookEvent`.
3. `billing_succeeded`: ONE transaction — promote row
   `pending → active` (guarded by `CanTransition`), set
   `current_period_start/end` and `provider_ref`, insert the
   `membership_events` ledger row (70/30 shares via `SplitShares`),
   accrue `creator_earnings.pending_paise += creator_share`.
4. `billing_failed`: row → `past_due`, ledger row with the failure;
   member keeps a grace window per plan; the aggregator's retry
   policy (not ours) drives recovery.
5. `cancelled` (member cancelled in their bank app, or we cancelled):
   row → `cancelled` (at period end if `atPeriodEnd`), ledger row,
   entitlement drops at (or past) `current_period_end`.

Idempotency: webhooks are idempotent on `(provider_ref, event id)` —
a retried `billing_succeeded` returns 200 to the aggregator and
changes nothing; retries must never double-accrue the creator.

Until wired, every step past row creation is a typed error: the
`pending` row sits honestly in the DB, the member sees "billing is
coming soon", and nothing pretends to charge.

## Payouts

Payouts ride the existing `payments.Provider.Payout` seam (F4): the
post-investors adapter sweeps `creator_earnings.pending_paise` per
creator on a schedule, settles via Cashfree payouts to the creator's
bank/UPI, and moves the settled amount to `paid_out_paise`. While both
seams are stubs, pending only grows — which is exactly the auditable
position the company wants to be in at incorporation time.

## Failure modes

| Situation | Behavior |
|---|---|
| `SUBSCRIPTIONS_PROVIDER` unknown (incl. `"cashfree"` pre-onboarding) | API refuses to boot (`os.Exit(1)`) — loud error, no half-state |
| Any stub method called | Typed `*subscriptions.ErrSubscriptionsDisabled` — explicit, never a fake |
| `CASHFREE_*` set but stub wired | Boot succeeds with a loud WARN: keys are ignored, no mandates can be created |
| Webhook with bad signature (real adapter) | Rejected — never normalized into an event |
| `billing_succeeded` retry (real adapter) | 200 to the aggregator, no double ledger row, no double accrual |
| `billing_failed` after `active` | Row → `past_due`, grace window; never silently cancels |

## Environment

| Var | Default | Purpose |
|---|---|---|
| `SUBSCRIPTIONS_PROVIDER` | `stub` | Only `stub` boots until the post-investors Cashfree Subscriptions + UPI AutoPay onboarding; anything else refuses to boot (fail-closed) |
| `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY` / `CASHFREE_ENV` | *(unset)* | Shared with the payments seam; loaded, never logged, ignored by the stub (WARN when set) |

## Post-investors onboarding checklist

Nothing below is wired today; this is the checklist that turns the
stub into the Cashfree Subscriptions adapter. Do not mark any item done
from a code read — each needs a live artefact.

- [ ] Signed Cashfree agreement + merchant account (legal entity must
  exist — post-incorporation).
- [ ] Subscriptions product activated on the merchant account; UPI
  AutoPay (e-mandate) enabled; mandate limits / max amount confirmed in
  writing from the current pricing sheet.
- [ ] API credentials issued for sandbox and prod separately; stored in
  the secret store, never in `.env`.
- [ ] Webhook endpoint registered with Cashfree; webhook secret stored;
  `VerifyWebhook` HMAC + timestamp + replay checks live-tested against
  sandbox callbacks (including replayed and tampered ones).
- [ ] Sandbox end-to-end: plan → subscribe → UPI AutoPay test mandate
  → `SUBSCRIPTION_PAYMENT_SUCCESS` webhook → row active → ledger row
  70/30 → accrue → cancel at period end → `cancelled`, all verified on
  the Cashfree dashboard.
- [ ] Idempotency proven: retried webhooks do not double-accrue.
- [ ] Payout flow proven: sweep → Cashfree payout → `paid_out_paise`
  moves; failure states (rejected bank account) handled.
- [ ] `SUBSCRIPTIONS_PROVIDER=cashfree` flipped in staging first, then
  prod; compose pin updated; the fail-closed WARN disappearing from
  boot logs is the observable "billing is live" signal.
