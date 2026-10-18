-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 0009: stable numeric tie-breaker for search keyset paging.
--
-- GET /v1/search pages with a true keyset on
-- (updated_at_unix, search_seq) — Meilisearch's <, <=, >, >= filters
-- work on NUMBERS only (per the official data-types doc), so the uuid
-- primary key cannot serve as the tie-breaker. search_seq is a
-- gapless-enough insertion sequence: unique per product, never
-- reassigned, so (updated_at, search_seq) is a total order and pages
-- never skip or duplicate a listing.
ALTER TABLE products
    ADD COLUMN search_seq BIGINT GENERATED ALWAYS AS IDENTITY;

-- Existing rows predate the column: backfill is automatic — the
-- identity default evaluates per row on ADD COLUMN. Uniqueness is
-- what the tie-breaker needs; the relative order of old rows is
-- irrelevant.
CREATE UNIQUE INDEX products_search_seq_unique ON products (search_seq);
