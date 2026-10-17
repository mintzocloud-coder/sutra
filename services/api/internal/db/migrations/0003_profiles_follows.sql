-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0003: profiles + follow graph (D2).
--
-- profiles holds the PUBLIC identity of a user: the first-come handle,
-- display name, bio, avatar. It is created lazily on the first PATCH
-- /v1/me/profile — never on registration — so reads of users without a
-- profile fall back to the users row (OIDC-seeded display name, NULL
-- handle). handle is claimed first-come: the UNIQUE constraint is the
-- single serialization point for concurrent claims, so two racers can
-- never hold the same handle (the loser gets a 409, see internal/social).
--
-- The placeholder users.handle column from migration 0002 is dropped
-- here: the profile endpoint it pointed at is this migration's profiles
-- table. users.display_name stays as the OIDC name seed for JIT
-- provisioning; a set profiles.display_name overrides it on reads.
--
-- follows is the directed social edge follower -> followee. Composite
-- primary key makes (un)follow idempotent at the SQL level; the
-- no_self_follow CHECK (also enforced in Go) keeps the graph clean.
-- created_at on the edge powers cursor pagination of followers/following
-- (keyset on (created_at, follower_id), never OFFSET) and can drive
-- follow-time sorting in timelines later.
--
-- Safe to re-run: IF NOT EXISTS + DROP IF EXISTS throughout.

-- Profiles table.
CREATE TABLE IF NOT EXISTS profiles (
    user_id      uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    handle       text        UNIQUE,
    display_name text,
    bio          text,
    avatar_url   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS profiles_set_updated_at ON profiles;
CREATE TRIGGER profiles_set_updated_at
    BEFORE UPDATE ON profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Follow graph.
CREATE TABLE IF NOT EXISTS follows (
    follower_id  uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id  uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT no_self_follow CHECK (follower_id <> followee_id)
);

-- Cursor pagination reads both directions of the edge, always ordered by
-- (created_at DESC, <other side> DESC).
CREATE INDEX IF NOT EXISTS follows_following_idx ON follows (follower_id, created_at DESC);
CREATE INDEX IF NOT EXISTS follows_followers_idx ON follows (followee_id, created_at DESC);

-- Retire the 0002 placeholder: handles live on profiles now.
ALTER TABLE users DROP COLUMN IF EXISTS handle;
