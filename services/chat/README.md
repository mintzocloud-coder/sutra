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
