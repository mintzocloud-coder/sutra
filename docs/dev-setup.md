# Dev setup — laptop to first commit in 10 minutes

## Prereqs

- Docker + Docker Compose
- Git
- (per service, later) Elixir 1.17+, Go 1.23+, Python 3.12+, Node 20+, JDK 17+

## One command

```bash
docker compose up
```

This brings up:

| Service | Port | What |
|---|---|---|
| PostgreSQL 18 + pgvector | 5432 | system of record (user `sutra` / password `sutra`) |
| Valkey | 6379 | cache + ephemeral state |
| SeaweedFS | 9333 / 8080 | S3-compatible blob storage |

## Verify

```bash
pg_isready -h localhost -p 5432 -U sutra
valkey-cli -p 6379 ping
curl -s localhost:9333/cluster/status
```

## First commit checklist

1. Read `ARCHITECTURE.md` and the ADRs in `docs/architecture/`.
2. Pick an issue — `good first issue` if you're new.
3. Sign the CLA (comment "I agree to the CLA" on your first PR).
4. SPDX header on every source file: `// SPDX-License-Identifier: AGPL-3.0-or-later`
5. CI must pass: `bash scripts/check-headers.sh` locally first.
