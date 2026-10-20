<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Sutra Ads Platform

Mintzo's ad platform: a custom Go core (campaigns, Generalized Second
Price auction, budgets, reporting) plus **Promovolve** (promovolve.org,
Apache-2.0, 2025–26, privacy-native contextual ad server) adopted for
the contextual-matching patterns/code. This is the deliberate license
split from the Wave-5 research: **Revive Adserver was REJECTED** —
GPL-2.0-only is license-incompatible with an AGPLv3 combined work,
PHP stack, declining community. Everything revenue-critical (auction,
billing, 55/45 creator split accounting) is owned in Go; Promovolve's
Apache-2.0 code/patterns cover only the non-core contextual-matching
component.

## Economics

- **Creator ad rev-share: 55% to creators, 45% to the platform** (founder
  directive, YouTube parity). The split accrues in the billing path
  (part 3); the accounting fields are documented here so money never
  gets designed twice.
- **Money is integer paise everywhere.** No floats in bids, budgets,
  prices, or splits.
- **0%-commission marketplace model applies to ads too**: the ads
  platform charges for impressions, not for sales commission.

## Components (all shipped unless marked)

| Piece | Location | Status |
|---|---|---|
| GSP auction kernel (bid → rank → price) | `internal/ads/auction.go` | ✅ shipped |
| Advertiser API (campaigns/budgets/targeting) | `internal/ads/campaigns.go`, `campaign_handler.go` | ✅ shipped |
| Creative registry | `internal/ads/creatives.go`, `creative_handler.go`, migration 0018 | ✅ shipped |
| Reporting v1 (accounting) | `internal/ads/report.go` | ✅ shipped |
| Serving decision (auction on feed slots) | calls `Eligible` + kernel + `ActiveCreatives` | 🔜 serving integration |
| Delivery events ledger (`ad_events`) | specified below | 🔜 serving integration |
| Billing (spend accrual, 55/45 split) | specified below | 🔜 serving integration |
| Contextual matching (Promovolve patterns) | `docs/ads.md` contract only | 🔜 |
| ClickHouse analytics pipeline | documented follow-on | 🔜 |

## Auction kernel

`internal/ads/auction.go` — deterministic Generalized Second Price:

- **Rank** = bid_paise × quality, descending. Quality is *consumed* by
  the kernel, never computed — the serving layer supplies it (Promovolve
  contextual score in the future; a constant until then).
- **Price**: the winner pays `ceil(displaced_score / own_quality) + 1`,
  floored at the reserve, never above its own bid. Classic GSP.
- **Junk exclusion**: bids below the reserve, zero quality, or junk are
  excluded *before* ranking, so junk can never set prices.
- **Deterministic tie-breaks**: score → quality → bidder ID. Fully
  replayable for billing disputes.
- **`Winner()`** convenience for single-slot feeds (rank the slots,
  price the top).

Reserve: ₹0.50 CPM-equivalent per impression slot — a reserve stops junk
bidders from pricing the market at zero.

## Advertiser API

Seller-scoped: the advertiser is the caller's seller row (`POST
/v1/sellers` first); non-sellers get 403 `not_a_seller`; one seller can
never see another's campaigns (404s leak nothing).

| Route | Method | Notes |
|---|---|---|
| `/v1/ads/campaigns` | POST/GET | create (draft) 201; list newest-first keyset |
| `/v1/ads/campaigns/{id}` | GET/PATCH/DELETE | read / presence-flag partial update / archive (terminal) |
| `/v1/ads/campaigns/{id}/activate` | POST | draft/paused → active; refuses exhausted budget + past `end_at` |
| `/v1/ads/campaigns/{id}/pause` | POST | active → paused |
| `/v1/ads/campaigns/{id}/resume` | POST | paused → active |
| `/v1/ads/campaigns/{id}/report` | GET | v1 accounting report (see below) |
| `/v1/ads/campaigns/{id}/creatives` | POST/GET | register draft creative / list |
| `/v1/ads/creatives/{id}` | GET/PATCH/DELETE | creative item (archive = terminal) |
| `/v1/ads/creatives/{id}/activate` | POST | draft → active creative |

- **Money**: `budget_paise` > 0 (≤ ₹10cr), `spend_paise` read-only
  (billing owns it), optional `daily_budget_paise` ≤ budget, budget can
  never be patched below current spend.
- **Targeting is a closed key set** — unknown keys are loud 422s. A
  typo'd key silently matching everything would spend real money.
  Keys: ISO-3166-1 countries, ISO-639-1 languages, real catalog
  category slugs (verified against the tree in-tx), mobile/tablet/
  desktop devices.
- **Eligibility** (`ads.Eligible`, pure, unit-tested): active + window
  open + spend < budget. The kernel must not depend on the status flip
  having landed — an exhausted budget stops serving even while the row
  still says active.

## Creative registry + serving contract

Migration 0018: creatives belong to a campaign, carry image/video
`media_id` (references the D4 media pipeline — a creative can only point
at `ready` media), click URL, and a draft → active → archived lifecycle.
`ActiveCreatives(campaignID)` is the serving path's inventory query.

**Serving flow (the integration to build):** for an ad slot →
`ActiveCreatives` per eligible campaign (`Eligible` checked at decision
time) → contextual quality score (Promovolve pattern: LLM matches the
ad to *page content*, no cookies/profiles; the publisher approves every
ad) → auction kernel → `Winner()` → record the delivery event →
accrue the GSP price to `spend_paise` → split 55/45.

The creative asset itself is never proxied through the API: the API
returns the media URL (SeaweedFS via the D4 pipeline); the client
renders it. Promovolve is the documented self-hosted option for the
full contextual-serving stack if the custom path ever needs one —
migration 0018 carries the reference.

## Reporting v1

`GET /v1/ads/campaigns/{id}/report` → `ads.BuildReport` (pure,
unit-tested): budget, spend, remaining (never negative), eligibility
now + machine-stable reason (`not_active` / `window_not_started` /
`window_closed` / `budget_exhausted` / `archived` — pinned in lockstep
with `Eligible` by test), days-live, spend-per-day, and a projected
exhaustion date. Projections degrade to **nulls, not guesses**, when
there is no spend history. Delivery analytics do not exist in v1:
`delivery.present: false` says so explicitly.

**Delivery ledger contract (for the serving integration):** when the
serving integration lands, it adds an `ad_events` table
(`impression_id` PK, `campaign_id`, `creative_id`, `slot`,
`price_paise` = the GSP price actually charged, `created_at`; index on
`(campaign_id, created_at)`), the report gains a `delivery` section
(impressions, billed spend reconciliation, CTR once clicks land), and
`spend_paise` is incremented in the same transaction as the event row.
The v1 report's JSON shape is forward-compatible: `delivery` is an
object whose `present` flips to true and gains fields.

## Billing (to build)

Per won impression: charge the GSP price (`price_paise` ≤ winner's bid,
≥ reserve), increment `campaign.spend_paise` atomically, credit 55% to
the creator's earnings ledger, 45% to the platform. Money movement rails
are the F4 Cashfree seam (fail-closed stub today — billing stays
ledger-only until post-investors onboarding, documented like the D1 OTP
adapter).

## Env vars

No ads-specific env vars exist yet. The serving integration will add
`ADS_SERVING_ENABLED` (default off, fail-closed like every other seam)
and `ADS_RESERVE_PAISE` (default 50).

## Failure modes

| Failure | Behavior |
|---|---|
| Serving layer down | no ads served; report still answers (accounting is DB-local) |
| Spend update races budget | `spend_paise <= budget_paise` CHECK + status predicate in Transition; over-budget edge 422s |
| Junk bids | excluded before ranking; can never set prices or win |
| Auction dispute | deterministic replay: same bids + qualities + reserve → same winner + price |

## Out of scope (Wave 2+)

Click tracking + CTR (needs the delivery ledger), frequency capping,
lookalike/identity targeting (philosophy: contextual > identity —
DPDP-era durable), real-time pacing controllers, self-serve advertiser
invoicing (F4 rails first).
