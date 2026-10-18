-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0004: posts table (D3, part 1: posts CRUD).
--
-- posts is the write-once-read-many content store behind the feed.
-- Every post belongs to an author (users.id, cascade on delete). The
-- body is plain text capped at 500 characters (Telegram parity); the
-- DB CHECKs are the backstop — the Go layer validates with rune counts
-- and returns 422s before we ever get here.
--
-- media_ids references media rows owned by the D4 media pipeline; until
-- D4 lands, entries are validated for SHAPE only (the readiness gate
-- "reference only ready media" is enforced in D4, documented there).
-- Max 10 media per post (sane product cap, enforced here + in Go).
--
-- created_at powers the author-timeline keyset ((author_id,
-- created_at DESC, id DESC) index) and the per-user post velocity cap
-- in D3 (rolling 1-hour count window). D3 part 2 adds Valkey sorted-set
-- home timelines (fan-out) on top of this table; this table stays the
-- source of truth and the pull path for high-follower authors.
--
-- Safe to re-run: IF NOT EXISTS throughout.

CREATE TABLE IF NOT EXISTS posts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id  uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    text       text        NOT NULL,
    media_ids  text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT posts_text_nonblank CHECK (btrim(text) <> ''),
    CONSTRAINT posts_text_maxlen CHECK (char_length(text) <= 500),
    CONSTRAINT posts_media_max CHECK (cardinality(media_ids) <= 10)
);

DROP TRIGGER IF EXISTS posts_set_updated_at ON posts;
CREATE TRIGGER posts_set_updated_at
    BEFORE UPDATE ON posts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Author timeline: newest first, keyset-paginated on
-- (created_at DESC, id DESC). Also serves the velocity-cap count query
-- (author_id, created_at > window) as an index-only range scan.
CREATE INDEX IF NOT EXISTS posts_author_timeline_idx
    ON posts (author_id, created_at DESC, id DESC);
