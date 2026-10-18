# Sutra AI core

**Status:** E2 personal-AI chat live — POST `/chat` (served by the Go proxy
as `/v1/ai/chat`): LangGraph agent (`recall → generate → persist`) with
PG-backed per-user conversation memory, versioned-YAML system prompt,
per-user rate limit + daily token budget, streaming AG-UI 1.0 SSE.
Model: Sarvam-30B via Ollama in dev (env-configurable), vLLM endpoint in
prod — both OpenAI-compatible. The E1 echo graph remains on
`POST /ag-ui/run`.

## Architecture

```
user client ──JWT──> Go API (/v1/ai/*, RequireAuth) ──Bearer AI_SERVICE_TOKEN──> sutra-ai
                                                                              ├─ POST /ag-ui/run (SSE, AG-UI 1.0 events)
                                                                              ├─ LangGraph agent graphs
                                                                              └─ Ollama (dev) / vLLM (prod)
```

Python **never** serves user clients directly — every AI request passes the
Go API's JWT auth first, then the proxy adds the internal service token and
forwards the verified subject as `X-Sutra-Subject`. The AI service 401s any
request without the service token (fail-closed; it refuses to boot without
`AI_SERVICE_TOKEN` at all).

## AG-UI protocol (endpoint: POST /ag-ui/run)

Request: `RunAgentInput` — `{thread_id, run_id, messages[], state{}}`.
Response: `text/event-stream` of AG-UI 1.0 BaseEvents:

```
RUN_STARTED → STEP_STARTED/STEP_FINISHED (per graph node)
            → TEXT_MESSAGE_START → TEXT_MESSAGE_CONTENT → TEXT_MESSAGE_END
            → RUN_FINISHED        (or RUN_ERROR, terminal, on failure)
```

E1 emits the chat-shaped subset over the echo graph. E2's `/chat` endpoint
emits the same vocabulary over the personal-AI graph (`recall`, `generate`,
`persist` nodes surface as STEP_STARTED/STEP_FINISHED; model deltas stream
as TEXT_MESSAGE_CONTENT via LangGraph's custom stream mode — true
streaming, not buffered). Tool calls (`TOOL_CALL_START/ARGS/END/RESULT`)
and shared state (`STATE_SNAPSHOT/DELTA`) arrive with later E-tasks behind
the same envelope, so clients written against E1/E2 keep working.

## Personal-AI chat (E2)

`POST /chat` → Go proxy → `/v1/ai/chat` (JWT-authenticated; the proxy
forwards the verified subject as `X-Sutra-Subject`, which the AI service
requires — no subject, no chat).

One turn:

```
validate → per-user rate limit (429 ai_rate_limited + Retry-After)
  → daily token budget pre-flight (429 ai_budget_exhausted + Retry-After)
  → recall: resolve (subject, thread_id) + load last N messages + attach
    the versioned system prompt
  → generate: stream the model, forwarding each delta as SSE
  → persist: save the turn + record real token usage → RUN_FINISHED
```

Guards run BEFORE streaming starts, so rejections are clean JSON 429s,
never truncated streams. Memory is PG-backed (`ai_threads`,
`ai_messages`, `ai_usage_daily`, `ai_rate_windows` — migrated by the
service itself at startup under an advisory lock, ledger
`ai_schema_migrations`; separate from the Go API's and chat service's
ledgers). Every assistant row stamps the `prompt_version` that produced
it. The persona lives ONLY in `prompts/system/{version}.yaml` — never
hardcoded; a rollout is a new YAML file + `AI_PROMPT_VERSION`.

## LangGraph agents

`app/graph.py` owns the graphs. The E1 graph is `START → echo → END`, a
real compiled `StateGraph` served on `POST /ag-ui/run`. The E2 personal-AI
graph is `recall → generate → persist`, served on `POST /chat`: recall
resolves the per-user thread and loads history + the versioned system
prompt; generate streams the model (deltas forwarded through LangGraph's
custom stream channel); persist saves the turn and records real token
usage for the daily budget. Both graphs are consumed through streaming
interfaces so the HTTP layer never touches graph internals.

## Model runtime

| Env | Runtime | Notes |
|-----|---------|-------|
| dev (compose) | Ollama (`ollama/ollama`), model `AI_MODEL` (default `sarvam-30b`) | Pull once: `docker compose exec ollama ollama pull sarvam-30b`. The client speaks OpenAI-compatible `POST {base}/chat/completions` with `stream: true`; `stream_options.include_usage` is requested so budgets record real counts. |
| prod | vLLM, OpenAI-compatible endpoint | Point `AI_MODEL_BASE_URL` at it (E2 reads it; `OLLAMA_BASE_URL` is the legacy fallback that becomes `{OLLAMA_BASE_URL}/v1`). |
| smoke/CI | Stub (`AI_MODEL_STUB=1`) | Deterministic canned replies marked as stub output — test-only, never production. |

Both speak the OpenAI chat-completions shape, so the E2 model client stays
runtime-agnostic.

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `PORT` | `8000` | Listen port inside the container |
| `AI_SERVICE_TOKEN` | *(required)* | Shared secret with the Go proxy; empty → service refuses to boot |
| `DATABASE_URL` | *(required)* | PG for memory/budgets/rate windows; empty → refuses to boot (E2) |
| `OLLAMA_BASE_URL` | `http://ollama:11434` | Legacy dev model runtime (derived to `{base}/v1` when `AI_MODEL_BASE_URL` unset) |
| `AI_MODEL_BASE_URL` | *(derived)* | OpenAI-compatible base URL (vLLM in prod) |
| `AI_MODEL` | `sarvam-30b` | Model name (env-configurable) |
| `AI_MODEL_STUB` | `0` | `1` = deterministic stub replies (smoke/CI only) |
| `AI_DAILY_TOKEN_BUDGET` | `50000` | Per-user daily token cap (UTC day); over → 429 `ai_budget_exhausted` |
| `AI_CHAT_RPM` | `20` | Per-user requests/minute (fixed window); over → 429 `ai_rate_limited` |
| `AI_AGUI_RPM` | `60` | Per-user requests/minute on `/ag-ui/run` (fixed window, E6); over → 429 `ai_rate_limited` (no token budget — echo spends zero tokens) |
| `AI_MAX_HISTORY` | `30` | Messages of history fed to the model per turn |
| `AI_MAX_INPUT_CHARS` | `8000` | Max user message length; over → 422 |
| `AI_PROMPT_VERSION` | `v1` | Selects `prompts/system/{version}.yaml` |

Go side: `AI_SERVICE_URL` (default `http://ai:8000`), `AI_SERVICE_TOKEN`
(same value). Token set + unparseable URL → API refuses to boot. Token
empty → `/v1/ai/*` returns `503` (envelope code `service_unavailable`,
message "ai service not configured"); token set + AI down at request
time → `503` + `Retry-After: 5`, message "ai service unavailable".

## Wave-2 roadmap (what plugs in where)

- **E2 personal AI chat** — DONE (this doc): `POST /v1/ai/chat` (SSE) via
  the LangGraph `recall → generate → persist` agent; PG conversation
  memory, versioned-YAML system prompt, per-user rate limit + daily token
  budget (both 429 with Retry-After); smoke covers the round-trip, memory
  persistence, and both 429 paths.
- **E3 creator studio** — faster-whisper worker consumes the D4 transcode
  pipeline; captions land as a SeaweedFS sidecar, exposed on media status.
- **E4 language bridge** — POST `/v1/ai/translate` (IndicTrans2, MIT),
  Valkey-cached by (hash, target); `?translate=xx` on the feed (best-effort).
- **E5 recsys v1** — non-ML ranking in Go at read time (recency decay +
  follow-graph boost + engagement velocity). pgvector embeddings are
  **issue #19, reserved for Hacktoberfest contributors** — the worker must
  not build them here.
- **E6 hardening** — rate limits/abuse guards for E1–E5, eval harness,
  OpenAPI for `/v1/ai/*`, production.md + benchmarks.md updates.

## Gated items (stay gated)

- Real SMS/DLT: post-incorporation (see `docs/identity.md`).
- E2E encryption: Wave 2 — never promised in code or docs.
