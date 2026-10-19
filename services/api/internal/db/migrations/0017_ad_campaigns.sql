-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0017: ad campaigns (H3 part 2, Wave 5 — advertiser API).
--
-- The advertiser record. Advertisers are sellers (seller-scoped routes);
-- the FK cascade keeps campaigns honest when a seller row disappears.
-- Money is integer paise (India-first platform). Creatives live in part 3
-- (Promovolve serving) — this migration covers campaigns, budgets and
-- targeting only.
--
-- Status lifecycle (enforced by CHECK + the Go transition matrix):
--   draft    — created, never entered auctions
--   active   — eligible for auctions (subject to Eligible() in ads)
--   paused   — temporarily out of auctions, resumable
--   archived — terminal; behaves as 404 everywhere (registry tombstone)
--
-- Budget guardrails (DB-enforced, never client trust):
--   budget_paise > 0; spend_paise <= budget_paise (a campaign whose
--   budget is exhausted stops serving — enforced here AND in Eligible);
--   daily_budget_paise NULL or <= budget_paise (daily accounting lands
--   in part 3's reporting; the cap is stored now so part 3 can't
--   create budgets the money model contradicts);
--   end_at > start_at when both are set.
CREATE TABLE ad_campaigns (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    advertiser_id uuid NOT NULL REFERENCES sellers(id) ON DELETE CASCADE,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'paused', 'archived')),
    budget_paise bigint NOT NULL CHECK (budget_paise > 0),
    spend_paise bigint NOT NULL DEFAULT 0 CHECK (spend_paise >= 0),
    daily_budget_paise bigint
        CHECK (daily_budget_paise IS NULL OR daily_budget_paise > 0),
    targeting jsonb NOT NULL DEFAULT '{}'::jsonb,
    start_at timestamptz,
    end_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (spend_paise <= budget_paise),
    CHECK (daily_budget_paise IS NULL OR daily_budget_paise <= budget_paise),
    CHECK (start_at IS NULL OR end_at IS NULL OR end_at > start_at),
    CHECK (char_length(name) BETWEEN 1 AND 128)
);

-- Per-advertiser newest-first listing: keyset on (created_at, id).
CREATE INDEX ad_campaigns_advertiser_idx
    ON ad_campaigns (advertiser_id, created_at DESC, id DESC);

-- Active-window sweeps (part 3's serving path): campaigns whose window
-- is currently open.
CREATE INDEX ad_campaigns_window_idx
    ON ad_campaigns (status, start_at, end_at)
    WHERE status = 'active';

DROP TRIGGER IF EXISTS ad_campaigns_set_updated_at ON ad_campaigns;
CREATE TRIGGER ad_campaigns_set_updated_at
    BEFORE UPDATE ON ad_campaigns
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
