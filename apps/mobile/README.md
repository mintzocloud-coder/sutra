# apps/mobile — Kotlin Multiplatform

**Status:** 🟡 Scaffold.

**Shared logic in Kotlin, native UI** (SwiftUI on iOS, Compose on Android). Not native-dual, not full shared UI — the verdict from the mobile research.

## Rules

- Auth: OIDC via Hydra (narrow-scoped clients per app surface).
- Realtime: Phoenix Channels client.
- Offline-first where it matters: client outbox + `last_seen_id` backfill (chat), cached feeds.
- Low-end Android is a first-class citizen — test on it, not just flagships.
