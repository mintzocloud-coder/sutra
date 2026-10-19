-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0015: mini-app registry (G3, Wave 4: mini-app runtime).
--
-- Mini-apps are third-party (or first-party) lightweight apps that run
-- INSIDE the Sutra client inside a sandboxed WebView/iframe (the Telegram
-- model — see docs/mini-apps.md): the platform never trusts their code,
-- they talk to the platform only through the permission-gated JS bridge
-- API, and the platform's source of truth for WHO an app is lives here,
-- not in whatever bundle the client fetched.
--
-- mini_apps.status lifecycle (G3 part 1: developer self-publish):
--   draft      — registered but invisible; only the developer sees it
--   published  — listed in the directory, installable
--   suspended  — hidden again (developer self-suspend; platform action
--                arrives in a later wave)
--   archived   — terminal; DELETE ends here (row kept for audit,
--                installs are cascade-deleted)
-- Allowed transitions: draft->published, published->suspended,
-- suspended->published, *->archived. Enforced by CHECK in Go plus the
-- status CHECK below; the Go layer owns the transition matrix.
--
-- The manifest column is the permission manifest: {"permissions": [...]}
-- validated against the bridge API allowlist in internal/miniapp (no
-- permission outside the allowlist can ever be declared — the bridge
-- denies undeclared permissions at runtime as defense-in-depth).
--
-- entry_url is the sandboxed frame's entry point. The platform fetches
-- NOTHING from it server-side in Wave 1 (no SSR proxying, no preview
-- rendering); icon_url/entry_url are plain metadata.
CREATE TABLE mini_apps (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    developer_id uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    slug         text        NOT NULL UNIQUE
                                CHECK (slug ~ '^[a-z0-9-]{3,40}$'),
    description  text        CHECK (description IS NULL OR char_length(description) BETWEEN 1 AND 500),
    icon_url     text        CHECK (icon_url IS NULL OR icon_url ~ '^https?://'),
    entry_url    text        NOT NULL CHECK (entry_url ~ '^https?://'),
    manifest     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    version      text        NOT NULL DEFAULT '1.0.0'
                                CHECK (char_length(version) BETWEEN 1 AND 32),
    status       text        NOT NULL DEFAULT 'draft'
                                CHECK (status IN ('draft', 'published', 'suspended', 'archived')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Slugs are first-come, first-served: the UNIQUE constraint is the race
-- arbiter (loser gets 409 slug_taken).
CREATE INDEX mini_apps_developer_id_idx ON mini_apps (developer_id);
-- The public directory: published-only feed, newest first. Partial
-- index keeps the hot listing query tiny; archived/suspended/draft
-- apps never appear here.
CREATE INDEX mini_apps_published_idx ON mini_apps (created_at DESC, id DESC)
    WHERE status = 'published';

DROP TRIGGER IF EXISTS mini_apps_set_updated_at ON mini_apps;
CREATE TRIGGER mini_apps_set_updated_at
    BEFORE UPDATE ON mini_apps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A user's installed shelf: which apps they launched/added.
-- Composite PK makes install idempotent by construction (ON CONFLICT
-- DO NOTHING → 200 on re-install). Only published apps are installable —
-- enforced in the Go service (no trigger: the rule lives with the
-- publish state machine). Uninstall is a plain DELETE (204 whether or
-- not the edge existed); archiving an app cascade-deletes its installs.
CREATE TABLE mini_app_installs (
    user_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    mini_app_id uuid        NOT NULL REFERENCES mini_apps(id) ON DELETE CASCADE,
    installed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, mini_app_id)
);

CREATE INDEX mini_app_installs_app_idx ON mini_app_installs (mini_app_id, installed_at DESC);
