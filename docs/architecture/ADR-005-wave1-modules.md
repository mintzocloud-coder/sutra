# ADR-005: Wave-1 module verdicts (foundation)

**Date:** 2026-10-17 · **Status:** Accepted (research: Oct 2026, fit-ranked, popularity ignored)

## Identity & auth — adopt Ory Kratos + Hydra (Apache-2.0) as plumbing; UI custom

Headless fits KMP+Next.js; Hydra is the single OIDC issuer → ONE identity across 30 modules (each a narrow-scoped OIDC client). Sessions: short-lived JWT access (JWKS) + opaque rotating refresh tokens with reuse detection. India: phone-OTP primary — **TRAI DLT registration is a launch workstream** (entity/header/template, unregistered ≈ 0% delivery). Passkeys = progressive enhancement.

## Social feeds — build custom (core)

No fit OSS feed engine exists (GetStream = proprietary → rejected). Hybrid fan-out: push <~10K followers, pull celebrities/inactives. Valkey sorted-set timelines (IDs only), Postgres source of truth, Elixir Broadway/Oban fan-out workers, Go feed-read API. Wave-1: heuristic ranking + impression logging (feeds Wave-2 training).

## Short video — custom pipeline from OSS plumbing

FFmpeg 8.x workers → HLS ladder (H.264 240p–720p for low-end Android) → **SeaweedFS** (Apache-2.0) → commercial CDN (procurement, Indian PoPs + origin shield). **Avoid MinIO** (maintenance mode since Dec 2025).

## Messaging — Elixir custom, E2E in Wave-2

Phoenix Channels + PubSub 2.2 (PG2, no Redis) + Presence (CRDT) + libcluster; partitioned PG18 append-only store with `client_msg_id`; client outbox + `last_seen_id` backfill for flaky networks. E2E via audited libsignal/MLS in Wave-2 — **do not promise E2E in Wave-1**; ship TLS + at-rest with the seams designed.
