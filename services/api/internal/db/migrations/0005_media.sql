-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0005: media table (D4, part 1: media model + presigned upload).
--
-- media is the registry for every user-uploaded asset (video or image).
-- Bytes live on SeaweedFS; this table tracks ownership, lifecycle
-- status, and (for videos) the HLS renditions the transcode worker
-- produces in D4 part 2. Posts reference media by id (posts.media_ids)
-- and may only reference READY rows — that gate is enforced in Go.
--
-- Lifecycle: pending (upload URL minted, no bytes yet) → uploaded
-- (client signalled completion, API verified bytes on SeaweedFS;
-- images are done here) → ready (images: at complete time; videos:
-- after the transcode worker finishes) → failed (worker gave up, error
-- carries the reason). Direct PUTs go straight to SeaweedFS, never
-- through the API, so the API stays out of the data path.
--
-- Safe to re-run: IF NOT EXISTS throughout.

CREATE TABLE IF NOT EXISTS media (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        text        NOT NULL,   -- 'video' | 'image'
    status      text        NOT NULL DEFAULT 'pending',
    fid         text,                   -- SeaweedFS file id, set at assign time
    filename    text,
    mime_type   text,
    size_bytes  bigint,                 -- set when the upload completes
    width       int,
    height      int,
    duration_ms bigint,                 -- video only, set by the worker
    poster_fid  text,                   -- video poster frame on SeaweedFS
    renditions  jsonb       NOT NULL DEFAULT '[]'::jsonb,
    error       text,                   -- failure reason when status = failed
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT media_kind_check   CHECK (kind IN ('video', 'image')),
    CONSTRAINT media_status_check CHECK (status IN ('pending', 'uploaded', 'ready', 'failed')),
    CONSTRAINT media_size_check   CHECK (size_bytes IS NULL OR size_bytes >= 0)
);

DROP TRIGGER IF EXISTS media_set_updated_at ON media;
CREATE TRIGGER media_set_updated_at
    BEFORE UPDATE ON media
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Owner's uploads, newest first (the "my media" listing future UIs need).
CREATE INDEX IF NOT EXISTS media_owner_created_idx
    ON media (owner_id, created_at DESC, id DESC);

-- Transcode worker sweep (D4 part 2): pending→stale and uploaded rows.
CREATE INDEX IF NOT EXISTS media_status_created_idx
    ON media (status, created_at);
