-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0006: auto-caption request surface (E3, part 1: creator
-- studio). Sidecars live on SeaweedFS (VTT + SRT fids); this table only
-- tracks the request lifecycle.
--
-- auto_caption is the sticky opt-in: set when the client asks for
-- captions (POST /v1/media/upload intent or POST /v1/posts
-- auto_caption=true). caption_status is the worker's lifecycle:
--   none → requested → processing → ready | failed
-- The caption worker (services/ai, part 2) consumes the media:captions
-- Valkey stream and moves rows through processing → ready/failed;
-- auto_caption stays true so a later "caption me" retry stays a no-op
-- once sidecars exist.
--
-- Safe to re-run: IF NOT EXISTS / guarded constraint / IF NOT EXISTS
-- index throughout.

ALTER TABLE media
    ADD COLUMN IF NOT EXISTS auto_caption   boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS caption_status text    NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS caption_vtt_fid text,
    ADD COLUMN IF NOT EXISTS caption_srt_fid text,
    ADD COLUMN IF NOT EXISTS caption_error  text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'media_caption_status_check'
    ) THEN
        ALTER TABLE media
            ADD CONSTRAINT media_caption_status_check
            CHECK (caption_status IN ('none', 'requested', 'processing', 'ready', 'failed'));
    END IF;
END $$;

-- Caption worker sweep (part 2): rows awaiting or mid-caption,
-- regardless of the media's own status (the worker only acts when the
-- row is ready; see docs/creator-studio.md for the contract).
CREATE INDEX IF NOT EXISTS media_caption_sweep_idx
    ON media (caption_status, updated_at)
    WHERE caption_status IN ('requested', 'processing');
