-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0019: ad serving core (H3 part 3b chunk 1, Wave 5).
--
-- The serving integration needs two things the earlier parts did not:
--   1. a per-campaign max bid (bid_paise) — the auction kernel needs a
--      real bid amount, and the campaign owner is the only one allowed to
--      set it. NULL = not bid-enabled: the campaign can never enter an
--      auction (fail-closed — a campaign without a bid cannot spend).
--   2. the ad_events delivery ledger (docs/ads.md "Delivery ledger
--      contract"): one row per served impression. spend_paise accrual
--      happens in the same transaction as the event row (serving.go);
--      the 55/45 creator/platform split is stored on the row, ledger-only
--      until F4's Cashfree onboarding (no money movement rails today).

ALTER TABLE ad_campaigns
    ADD COLUMN bid_paise bigint
        CHECK (bid_paise IS NULL OR bid_paise > 0);

COMMENT ON COLUMN ad_campaigns.bid_paise IS
    'Max paise the advertiser will pay per won impression. NULL = not bid-enabled (fail-closed: never enters auctions).';

-- Delivery ledger: append-only, one row per served impression.
-- price_paise is the GSP price actually charged (<= winner bid, >= reserve).
-- creator_share_paise = floor(price * 55 / 100); platform takes the rest.
CREATE TABLE ad_events (
    impression_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id   uuid NOT NULL REFERENCES ad_campaigns (id) ON DELETE CASCADE,
    creative_id   uuid NOT NULL REFERENCES ad_creatives (id) ON DELETE CASCADE,
    slot          text NOT NULL CHECK (slot <> ''),
    price_paise   bigint NOT NULL CHECK (price_paise > 0),
    creator_share_paise bigint NOT NULL CHECK (creator_share_paise >= 0),
    platform_share_paise bigint NOT NULL CHECK (platform_share_paise >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (creator_share_paise + platform_share_paise = price_paise)
);

CREATE INDEX ad_events_campaign_created_idx
    ON ad_events (campaign_id, created_at);

COMMENT ON TABLE ad_events IS
    'Append-only delivery ledger for served impressions (H3 part 3b). spend_paise is incremented in the same transaction as each event row.';
