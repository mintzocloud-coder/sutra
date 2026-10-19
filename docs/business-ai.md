# SPDX-License-Identifier: AGPL-3.0-or-later
# Business AI (H1) — the seller copilot on the Wave-2 stack.

## What it is

`POST /business` on the AI service (served by the Go proxy as
`POST /v1/ai/business`): a LangGraph agent (`recall → retrieve →
generate → persist`) that answers two kinds of seller questions:

- **Catalog Q&A** — "what is the price of the red kurta?", "which SKUs
  are out of stock?" — answered ONLY from the seller's own active
  catalog rows.
- **Order insights** — "revenue this month?", "orders by status" —
  answered ONLY from the seller's own orders.

It reuses the Wave-2 machinery: AG-UI 1.0 SSE event vocabulary,
PG-backed threads/memory (`ai_threads`, shared with personal chat),
the PG fixed-window rate limiter + daily token budget (the budget pool
is SHARED with `/chat` — one user, one pool), the versioned-prompt
pattern (`prompts/system/business_v1.yaml`, never hardcoded), and the
OpenAI-compatible model client (Ollama dev / vLLM prod / stub for
smoke-CI).

## The "never invent attributes" guardrail (three layers)

A wrong price shown to a seller destroys trust in the whole platform,
so attribute grounding is enforced, not hoped for:

1. **Prompt** — `prompts/system/business_v1.yaml` states the rule, the
   exact sentence to use when a fact is missing, and the DPDP privacy
   boundaries.
2. **Code** — `GroundingGuard` (`app/business.py`, pure, 26-test
   suite): the generate node BUFFERS the model reply, extracts every
   stated price (normalized to paise: `₹1,299` == `₹1299.00`) and every
   `SKU <id>` mention, and requires each to appear in the injected
   grounding block. A violation swaps the reply for a safe fallback —
   the fabricated attribute never reaches the wire, not even partially.
   Streaming starts only AFTER verification (time-to-first-token is
   deliberately traded for grounding certainty on money facts).
3. **Eval** — `eval/golden/v1_business.yaml`: stub-backend plumbing
   cases plus `real`-backend grounded cases (assert the true price is
   stated and a fabricated price is absent).

The guard mechanically covers **price and SKU** — the two attribute
types with machine-checkable identity. Counts, titles, and prose are
prompt+eval enforced: guarding bare numbers false-positives on
ordinary text ("top 5 products") without grounding value. The renderer
(`bizdata.render_grounding_block`) is the SINGLE source of facts for
both injection and validation, so they cannot drift apart.

## Data access: read-only by construction

The copilot reads the Go API's commerce tables through
`app/bizdata.py`. Every query block runs inside an explicit
`BEGIN READ ONLY` transaction: a bug that tried to write would die
with Postgres 25006 instead of corrupting commerce state. Reads are
strictly seller-scoped (`users.subject → sellers.user_id`); a caller
with no seller row gets `403 not_a_seller` BEFORE any store data is
touched (eager, in the service layer — the graph never runs for
non-sellers).

Intent routing (`classify_intent`, pure) decides which sources a turn
needs: `{"catalog"}`, `{"insights"}`, both, or neither (conversational
turns get no grounding block). Catalog keyword search strips question
framing and intent words (`product_keywords`): "what is the price of
red kurta?" searches `["red", "kurta"]`. A catalog question naming no
product ("show my products") lists the 5 most recently updated actives
instead of a blind keyword miss — and an empty keyword set never dumps
the catalog.

## DPDP notes (duties live May 2027)

India's DPDP Act enforcement duties take effect May 2027; the copilot
is designed to be compliant on day one:

- **Purpose limitation** — seller data is processed ONLY to answer
  that seller's business questions. No cross-seller comparison, no
  training on seller data, no secondary use. Documented in the system
  prompt and enforced by seller-scoped queries.
- **Data minimization** — read-only access, 5-product retrieval cap,
  6000-char grounding ceiling, 10-message history window (shorter than
  personal chat: business threads are task-oriented).
- **No buyer PII in answers** — insights are aggregates (status
  counts, revenue, top products by units). The prompt forbids disclosing
  buyer details beyond what the block shows (it shows none).
- **Consent** — using the copilot is consent to process the seller's
  own store data for their own questions; withdrawing = not using it.
  Consent records for any future outbound sharing live with the Go API's
  identity layer (Kratos), not here.
- **Breach notification / grievance** — platform-level, owned by the
  Go API + ops runbook (`docs/production.md`); this service logs
  grounding violations (thread id + subject) for guardrail tuning —
  operational telemetry, not personal data export.

## Failure modes

| Situation | Behavior |
|---|---|
| Not a seller | 403 `not_a_seller` JSON (eager, before guards) |
| Rate over AI_BIZ_RPM | 429 `ai_rate_limited` + Retry-After (JSON, never truncated SSE) |
| Daily budget exhausted | 429 `ai_budget_exhausted` + Retry-After (shared pool with /chat) |
| Model invents a price/SKU | Safe fallback served; violation logged (thread, subject) |
| Commerce DB unreachable | 500 via RUN_ERROR inside SSE after guards (guards are PG-backed too) |
| No products match | Grounding block says so; model is instructed to say exactly that |

## Environment

| Variable | Default | Notes |
|---|---|---|
| `AI_BIZ_RPM` | 20 | Per-user fixed-window rate limit for /business |
| `AI_BIZ_PROMPT_VERSION` | `business_v1` | Roll out persona changes as new YAML + flip |
| `AI_BIZ_MAX_HISTORY` | 10 | History window (business threads are short) |
| `AI_BIZ_MAX_GROUNDING_CHARS` | 6000 | Grounding-block ceiling (truncation noted in-block) |

Daily token budget (`AI_DAILY_TOKEN_BUDGET`) is shared with `/chat`.

## Roadmap (later H1 parts / Wave 5+)

- smoke.sh H1 round-trip (stub model): register seller → product →
  variant → ask grounded question → assert the true price in the SSE
  text; 403 for a non-seller; 429 paths. (Next part.)
- Real-model golden eval curation (human-graded, per prompt-version
  regression diffs) — Wave 2 eval maturity.
- Proactive insights (daily digest over the event bus) — needs seller
  opt-in consent records first (DPDP purpose limitation).
- Multi-store accounts (one user, N stores) — the resolve chain is
  ready; the API model is still one-seller-per-user (F1 decision).
