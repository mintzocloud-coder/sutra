# services/chat — Elixir/OTP realtime core

**Status:** 🟡 Scaffold — see issue #5 for the first slice (Phoenix Channels + Presence skeleton).

## What lives here (and ONLY here)

- Chat messaging (1:1 and groups) — channels, delivery, receipts
- Presence (online/last-seen, CRDT-based)
- Livestream orchestration and call signaling
- Push notification fan-out
- Feed fan-out workers (Broadway/Oban)

## Why Elixir, why narrow

Elixir's rarity is deliberate: thin talent pool = harder to copy. The footprint stays **narrow** (realtime core only) so the contributor funnel doesn't choke — broad contributions happen in Go/Python/Next.js. See `docs/architecture/ADR-004-elixir-moat.md`.

## Rules

- Phoenix Channels + PubSub 2.2 (PG2 adapter — **no Redis needed**).
- Message store: partitioned PostgreSQL 18 append-only tables, `client_msg_id` idempotency.
- E2E encryption is a **Wave-2** goal (libsignal/MLS) — ship TLS + at-rest first, design the seams now.
- Every new module: supervision tree first, tests with it.

## Running

- **Dev:** `mix phx.server` — websocket on `:4000`. Postgres defaults live
  in `config/config.exs`; override with `DATABASE_URL`.
- **Prod:** `mix release` image only — `mix phx.server` is never used in
  production. The Dockerfile builds the release, drops to a non-root user,
  and boots with `bin/sutra_chat start`. Required env: `DATABASE_URL` and
  `SECRET_KEY_BASE` (generate with `mix phx.gen.secret`; the release fails
  closed without it). First boot against a fresh database:
  `bin/sutra_chat eval "SutraChat.Release.migrate()"`.
