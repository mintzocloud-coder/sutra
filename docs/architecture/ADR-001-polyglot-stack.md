# ADR-001: Hybrid polyglot stack — Elixir + Go + Python

**Date:** 2026-10-17 · **Status:** Accepted · **Decider:** Harshal Vanpariya

## Decision

Sutra is built polyglot, with each language owning what it's best at:

| Layer | Language | Owns |
|---|---|---|
| Realtime core | **Elixir / Erlang OTP** | chat, presence, live orchestration, call signaling, notifications |
| Request serving | **Go** | core APIs, marketplace, orders, mini-app runtime, admin BFF |
| Intelligence | **Python** | recsys, AI agents, creator tools, moderation (off the hot path) |
| Web | **Next.js** (TypeScript) | marketing site + admin, self-hosted |
| Mobile | **Kotlin Multiplatform** | shared logic + native UI (SwiftUI/Compose) |

## Rationale

Follows the proven WhatsApp/Discord/TikTok pattern: BEAM for connection state, Go for request throughput, Python for ML. Seams: Phoenix Channels, NATS event bus, gRPC+protobuf, Postgres as system of record, Valkey for ephemeral state.

## Consequences

- Contributors need one `docker compose up`, not five toolchains — the monorepo + compose hides polyglot ops.
- Elixir footprint stays **narrow** (see ADR-004) so hiring rarity doesn't choke contributions.
