# services/api — Go core APIs

**Status:** 🟡 Scaffold — see issues #4 (Ory Kratos+Hydra spike) and #7 (feed fan-out RFC).

## What lives here

- Identity service (Ory Kratos + Hydra as plumbing, custom UI/flows)
- Social graph + feed-read APIs
- Marketplace engine (custom — **no readymade**; see Wave-3)
- Mini-app runtime, seller center, logistics abstraction
- Admin BFF

## Rules

- Go owns **request serving**. Realtime state lives in Elixir; intelligence lives in Python.
- PostgreSQL 18 is the system of record — one service, one schema, no cross-service table writes.
- Every endpoint: OpenAPI spec first, then code.
- Dependencies must be self-hostable and AGPLv3-compatible.
