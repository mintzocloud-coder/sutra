-- SPDX-License-Identifier: AGPL-3.0-or-later

-- I3: per-conversation E2E flag. Rooms live in the CHAT service in Wave 1
-- (open rooms by ID, see docs/room-membership.md), so the chat service is
-- the WRITER of this table: when it persists the first valid
-- e2e:session_init for a room, it flips e2e_enabled (monotonic — the flag
-- never flips back; first_init_message_id pins the first init row as the
-- evidence anchor). The Go API only READS it, exposed at
-- GET /v1/conversations/{room_id}/e2e.
--
-- Clients must treat "unknown room" (no row) as NOT E2E — never claim
-- E2E on a conversation where the flag is not set.

CREATE TABLE IF NOT EXISTS e2e_conversations (
    room_id TEXT PRIMARY KEY,
    e2e_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    enabled_at TIMESTAMPTZ,
    first_init_message_id UUID,
    CONSTRAINT e2e_conversations_room_id_sane
        CHECK (room_id ~ '^[A-Za-z0-9_-]{1,64}$')
);

COMMENT ON TABLE e2e_conversations IS
    'I3: per-conversation E2E flag. Written by the chat service when it observes the first valid e2e:session_init in a room; read-only for the Go API.';
COMMENT ON COLUMN e2e_conversations.e2e_enabled IS
    'True once X3DH session establishment was observed in the room. Monotonic: never set back to false.';
COMMENT ON COLUMN e2e_conversations.first_init_message_id IS
    'The messages.id of the first persisted e2e:session_init — the evidence anchor for the flag.';
