-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0018: ad creatives (H3 part 3a, Wave 5 — creative registry).
--
-- The creative registry: every ad unit an advertiser runs references a
-- creative row. Promovolve (Apache-2.0) is the documented self-hosted
-- creative-serving option (docs/ads.md lands in part 3b); this registry
-- is the source of truth the Go serving path and any external server
-- both read from.
--
-- Creative model (enforced here AND in the Go service — never trust the
-- client with serving inputs):
--   kind   — image | video | native (closed set; IAB-style registry).
--   format — closed per-kind size registry (image_1200x628,
--             image_1080x1080, image_1080x1920, video_1920x1080,
--             video_1080x1920, native). kind/format compatibility is a
--             CHECK (video_ formats only on video creatives, etc.).
--   media_url / landing_url — http(s) only (javascript:/data: can never
--             be served — XSS-by-creative is a real attack).
--   cta    — closed call-to-action registry (shop_now, learn_more,
--             install, sign_up, watch_more).
--   status — draft → active → archived (terminal). Archived creatives
--             are registry tombstones (404 everywhere); the serving path
--             only reads active creatives on active campaigns.
--
-- Money honesty: creatives carry no money fields — budgets live on the
-- campaign (0017). An active creative on an exhausted campaign never
-- serves (the Eligible() check in internal/ads owns that gate).
CREATE TABLE ad_creatives (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id uuid NOT NULL REFERENCES ad_campaigns(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL
        CHECK (kind IN ('image', 'video', 'native')),
    format text NOT NULL
        CHECK (format IN ('image_1200x628', 'image_1080x1080', 'image_1080x1920',
                          'video_1920x1080', 'video_1080x1920', 'native')),
    media_url text NOT NULL,
    landing_url text NOT NULL,
    headline text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    cta text NOT NULL DEFAULT ''
        CHECK (cta IN ('', 'shop_now', 'learn_more', 'install', 'sign_up', 'watch_more')),
    status text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'archived')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- kind/format compatibility: the registry never holds a
    -- 1080x1920 image declared as native, etc.
    CHECK (
        (kind = 'image' AND format LIKE 'image\_%') OR
        (kind = 'video' AND format LIKE 'video\_%') OR
        (kind = 'native' AND format = 'native')
    ),
    CHECK (char_length(name) BETWEEN 1 AND 128),
    CHECK (char_length(headline) <= 80),
    CHECK (char_length(description) <= 140),
    -- http(s) only at the storage layer too (the LIKE is a backstop for
    -- the Go validator; data:/javascript: must never be servable).
    CHECK (media_url LIKE 'http://%' OR media_url LIKE 'https://%'),
    CHECK (landing_url LIKE 'http://%' OR landing_url LIKE 'https://%')
);

-- Per-campaign newest-first listing: keyset on (created_at, id).
CREATE INDEX ad_creatives_campaign_idx
    ON ad_creatives (campaign_id, created_at DESC, id DESC);

-- Serving-path sweep (part 3b): active creatives per campaign.
CREATE INDEX ad_creatives_serving_idx
    ON ad_creatives (campaign_id, status)
    WHERE status = 'active';

DROP TRIGGER IF EXISTS ad_creatives_set_updated_at ON ad_creatives;
CREATE TRIGGER ad_creatives_set_updated_at
    BEFORE UPDATE ON ad_creatives
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
