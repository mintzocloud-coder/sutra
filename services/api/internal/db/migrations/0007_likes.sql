-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0007: likes (E5, recommendations v1). Likes are the only
-- engagement signal in Wave 2 v1 — the raw material for the
-- "engagement velocity" term of the home-feed ranker (see
-- docs/recsys.md). Non-ML by design: issue #19 (pgvector embeddings
-- pipeline) stays reserved for Hacktoberfest contributors and must
-- NOT touch this table.
--
-- Composite PK (post_id, user_id): a user likes a post at most once,
-- and like is idempotent by construction (INSERT ... ON CONFLICT
-- DO NOTHING). created_at drives the trailing-24h velocity window.
-- FKs CASCADE: deleting a post or a user removes their likes.

CREATE TABLE likes (
    post_id    uuid NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (post_id, user_id)
);

-- "Posts liked by user" (future: liked-posts timeline).
CREATE INDEX likes_user_idx ON likes (user_id, created_at DESC, post_id DESC);
