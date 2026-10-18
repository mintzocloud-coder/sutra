-- SPDX-License-Identifier: AGPL-3.0-or-later
-- 0001: E2 personal-AI chat — conversation memory, token budgets, rate windows.
--
-- The AI service owns this schema (migrated by app/db.py at startup with an
-- advisory lock, ledger ai_schema_migrations). It is deliberately separate
-- from the Go API's schema_migrations and the chat service's
-- chat_schema_migrations: each service migrates only its own tables.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- One row per (user, client thread). Threads are per-user: the AI service
-- keys everything on the verified JWT subject the Go proxy forwards as
-- X-Sutra-Subject; it never sees the user's JWT.
CREATE TABLE ai_threads (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject          TEXT NOT NULL,
    client_thread_id TEXT NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (subject, client_thread_id)
);
CREATE INDEX ai_threads_subject_idx ON ai_threads (subject, updated_at DESC);

-- Every turn's user + assistant messages. History is loaded oldest-first
-- with a LIMIT (never OFFSET); prompt_version records which system prompt
-- produced the assistant reply (auditability across prompt rollouts).
CREATE TABLE ai_messages (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id         UUID NOT NULL REFERENCES ai_threads(id) ON DELETE CASCADE,
    role              TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
    content           TEXT NOT NULL,
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    prompt_version    TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ai_messages_thread_idx ON ai_messages (thread_id, created_at ASC, id ASC);

-- Daily token consumption per user (UTC day). The budget guard reads this
-- before every turn and rejects with 429 ai_budget_exhausted when the day's
-- budget would be exceeded.
CREATE TABLE ai_usage_daily (
    subject           TEXT NOT NULL,
    day               DATE NOT NULL,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (subject, day)
);

-- Fixed one-minute rate-limit windows per user. The guard upserts the
-- current minute's row and rejects with 429 ai_rate_limited over the cap.
CREATE TABLE ai_rate_windows (
    subject      TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    count        INT NOT NULL DEFAULT 0,
    PRIMARY KEY (subject, window_start)
);
