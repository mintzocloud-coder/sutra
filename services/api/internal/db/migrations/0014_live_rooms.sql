-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0014: live room registry (G1, Wave 4: LiveKit).
--
-- The LiveKit SFU itself is stateless for our purposes: the API mints
-- room-scoped participant JWTs (internal/live) and LiveKit tracks the
-- actual media sessions. This table is the platform's durable record of
-- WHO is allowed to be live and WHAT is currently on air — the API never
-- trusts LiveKit's in-memory state for platform decisions (room listing,
-- entitlements, moderation) and LiveKit is never trusted for platform
-- state either.
--
-- A room is created by the host via POST /v1/live/rooms (status=live),
-- and ended via POST /v1/live/rooms/{id}/end (status=ended). The LiveKit
-- webhook ingress (G1 part 2) reconciles this table against SFU truth
-- (room_finished → ended) so crashed hosts don't leave rooms live
-- forever.
--
-- ROOM NAME: the LiveKit room identifier on the wire is
-- "sutra:<room_id>" (the DB id, not the display name) so display names
-- can be free-text without sanitisation risk. live:<name> collisions are
-- impossible by construction.
--
-- LIFECYCLE (live_rooms.status):
--   live   — the host is (or should be) on air; viewers may mint tokens
--   ended  — terminal; the host or the SFU webhook closed it
CREATE TABLE live_rooms (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       text        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    kind        text        NOT NULL DEFAULT 'livestream'
                            CHECK (kind IN ('livestream', 'call')),
    status      text        NOT NULL DEFAULT 'live'
                            CHECK (status IN ('live', 'ended')),
    viewer_cap  integer     NOT NULL DEFAULT 10000 CHECK (viewer_cap > 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz
);

-- Hosts list their own rooms; viewers list what's on air.
CREATE INDEX live_rooms_host_id_idx ON live_rooms (host_id);
-- The on-air feed: partial index keeps the hot query tiny.
CREATE INDEX live_rooms_live_idx ON live_rooms (created_at DESC)
    WHERE status = 'live';
