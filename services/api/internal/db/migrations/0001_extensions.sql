-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0001: baseline extensions.
--
-- pgcrypto provides gen_random_uuid(), used for UUID primary keys
-- (e.g. the users table landing in migration 0002 / task A4).
-- Safe to re-run: IF NOT EXISTS.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
