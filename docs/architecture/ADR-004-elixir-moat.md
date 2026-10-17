# ADR-004: Elixir rarity as a deliberate moat (narrow footprint)

**Date:** 2026-10-17 · **Status:** Accepted · **Decider:** Harshal Vanpariya

## Decision

Elixir's thin talent pool is treated as a **feature, not a bug**: fewer Elixir developers means competitors can't clone the realtime core quickly. The project's AI (Dhanasvi) is the Elixir muscle, so hiring scarcity doesn't block the originating team.

## Constraints (honest)

- Obscurity is a **speed-bump, not a wall** — a determined competitor can learn Elixir. The real moat stays network/data/brand/execution.
- Therefore the Elixir footprint stays **narrow**: realtime core only (chat, presence, live orchestration). Broad contribution surface stays Go/Python/Next.js so the contributor funnel doesn't choke.

## Consequences

- `services/chat` is Elixir-only and serious-contributors-only.
- If Elixir ever blocks velocity, the narrow footprint makes a rewrite tractable.
