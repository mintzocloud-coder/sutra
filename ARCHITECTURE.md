# Sutra monorepo layout

```
sutra/
├── apps/
│   ├── web/            # Next.js — marketing site + admin console (self-hosted, no Vercel-only features)
│   └── mobile/         # Kotlin Multiplatform — shared logic, native UI (SwiftUI/Compose)
├── services/
│   ├── chat/           # ELIXIR/OTP — realtime core: chat, presence, live orchestration, notifications (NARROW)
│   ├── api/            # GO — core APIs: identity, social graph, feed-read, marketplace, mini-app runtime
│   └── ai/             # PYTHON — intelligence, off the hot path: recsys, agents, creator tools, moderation
├── docs/
│   ├── architecture/   # ADRs — every big decision, written down
│   └── dev-setup.md    # laptop setup guide
├── scripts/            # check-headers.sh, dev helpers
├── .github/            # CI + issue templates
├── brand.config.json   # single source of truth for branding (codename now, brand at registration)
├── docker-compose.yml  # one-command dev stack
└── LICENSE / CLA.md / CONTRIBUTING.md / NOTICE
```

## Service seams (read before you code)

- **Elixir** owns connection state. **Go** owns request serving. **Python** owns intelligence.
- Talk across services via **NATS** (events), **gRPC + protobuf** (calls). Phoenix Channels for realtime clients.
- **PostgreSQL 18** is the system of record. **Valkey** is ephemeral state. **SeaweedFS** is blob storage.
- No service writes another service's tables. Ever.
- Every dependency must be **self-hostable** (`docker compose up`) and **AGPLv3-compatible** (no BSL/source-available).
