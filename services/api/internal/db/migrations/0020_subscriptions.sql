-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0020: subscriptions core (H4, Wave 5).
--
-- Creator memberships: a creator defines paid membership plans (tiers);
-- members subscribe via Cashfree Subscriptions with UPI AutoPay. The
-- platform take is 30% (locked economics, 2026-10-17); the creator keeps
-- 70%. The membership_events ledger records every billing outcome with
-- the 70/30 split stored on the row — ledger-only until the Cashfree
-- onboarding completes post-investors (no money-movement rails today,
-- same convention as the H3 ad_events ledger). creator_earnings carries
-- the accrual target; payouts ride payments.Provider.Payout once the
-- aggregator is live (F4).
--
-- While the fail-closed stub is wired, no subscription can leave
-- "pending": provider calls return *ErrSubscriptionsDisabled, so the
-- only reachable states are pending/cancelled-by-timeout — never
-- "active with fake money".

-- A membership plan is owned by exactly one creator (users.id).
-- price_paise > 0 and currency pinned to INR (India-first launch,
-- same convention as internal/payments).
CREATE TABLE subscription_plans (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name            text NOT NULL CHECK (name <> '' AND char_length(name) <= 80),
    description     text NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    price_paise     bigint NOT NULL CHECK (price_paise > 0),
    billing_period  text NOT NULL CHECK (billing_period IN ('monthly', 'yearly')),
    currency        text NOT NULL DEFAULT 'INR' CHECK (currency = 'INR'),
    is_active       bool NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX subscription_plans_creator_active_idx
    ON subscription_plans (creator_id, is_active) WHERE is_active;

-- keep the existing updated_at discipline (see 0002 trigger set_updated_at)
CREATE TRIGGER subscription_plans_set_updated_at
    BEFORE UPDATE ON subscription_plans
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE subscription_plans IS
    'Creator membership tiers (H4). One plan per tier; members subscribe via Cashfree Subscriptions + UPI AutoPay once the provider is live.';

-- Subscriptions: one row per member-plan billing relationship.
-- status lifecycle: pending -> active -> cancelled | past_due -> active |
-- cancelled. "pending" means the aggregator call has not succeeded; the
-- fail-closed stub can never promote past pending.
-- provider_ref is the aggregator subscription id (Cashfree subscription
-- id) once wired; NULL until then (fail-closed: NULL means no mandate
-- exists anywhere, so nothing can be charged).
-- The partial unique index guarantees a member cannot hold two live
-- subscriptions on the same plan (no double-billing surface).
CREATE TABLE subscriptions (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_id               uuid NOT NULL REFERENCES subscription_plans (id) ON DELETE RESTRICT,
    status                text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'active', 'past_due', 'cancelled')),
    provider_ref          text CHECK (provider_ref IS NULL OR provider_ref <> ''),
    current_period_start  timestamptz,
    current_period_end    timestamptz,
    cancel_at_period_end  bool NOT NULL DEFAULT false,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (current_period_end IS NULL OR current_period_start IS NULL OR current_period_end > current_period_start)
);

CREATE UNIQUE INDEX subscriptions_one_live_per_plan_idx
    ON subscriptions (subscriber_id, plan_id)
    WHERE status IN ('active', 'past_due');

CREATE INDEX subscriptions_subscriber_status_idx
    ON subscriptions (subscriber_id, status);

CREATE INDEX subscriptions_provider_ref_idx
    ON subscriptions (provider_ref) WHERE provider_ref IS NOT NULL;

CREATE TRIGGER subscriptions_set_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE subscriptions IS
    'Member-plan billing relationships (H4). provider_ref NULL = no aggregator mandate exists (fail-closed).';

-- Append-only membership billing ledger. One row per billing outcome
-- (billing_succeeded, billing_failed, cancelled): creator_share_paise =
-- floor(amount * 70 / 100), platform takes the rest (30% platform cut,
-- locked founder economics). The service writes this row and the
-- creator_earnings accrual in one transaction.
CREATE TABLE membership_events (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id       uuid NOT NULL REFERENCES subscriptions (id) ON DELETE CASCADE,
    plan_id               uuid NOT NULL REFERENCES subscription_plans (id) ON DELETE RESTRICT,
    event_type            text NOT NULL
        CHECK (event_type IN ('billing_succeeded', 'billing_failed', 'cancelled')),
    amount_paise          bigint NOT NULL CHECK (amount_paise > 0),
    creator_share_paise   bigint NOT NULL CHECK (creator_share_paise >= 0),
    platform_share_paise  bigint NOT NULL CHECK (platform_share_paise >= 0),
    provider_ref          text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (creator_share_paise + platform_share_paise = amount_paise),
    CHECK (event_type <> 'billing_succeeded' OR provider_ref IS NOT NULL)
);

CREATE INDEX membership_events_plan_created_idx
    ON membership_events (plan_id, created_at);

CREATE INDEX membership_events_subscription_created_idx
    ON membership_events (subscription_id, created_at);

COMMENT ON TABLE membership_events IS
    'Append-only membership billing ledger (H4): 70% creator / 30% platform on every row. Ledger-only until the Cashfree onboarding (F4); no money moves today.';

-- Per-creator accrual target for membership payouts. pending_paise grows
-- with each billing_succeeded event; paid_out_paise grows with real
-- payments.Provider payouts once the aggregator is live. While disabled,
-- payouts are impossible and pending only grows — an honest, auditable
-- record of what the platform owes, not a transfer.
CREATE TABLE creator_earnings (
    creator_id      uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    pending_paise   bigint NOT NULL DEFAULT 0 CHECK (pending_paise >= 0),
    paid_out_paise  bigint NOT NULL DEFAULT 0 CHECK (paid_out_paise >= 0),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER creator_earnings_set_updated_at
    BEFORE UPDATE ON creator_earnings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE creator_earnings IS
    'Creator membership accruals (H4). pending_paise is what the platform owes; paid_out_paise only moves via the payments seam (F4).';
