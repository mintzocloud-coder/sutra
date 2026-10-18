# Sutra AI core

**Status:** E1 scaffold live — FastAPI service, AG-UI endpoint skeleton over a
real LangGraph echo graph, Go API proxy at `/v1/ai/*`. The model, memory, and
tools land in E2–E5.

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

E1 emits the chat-shaped subset over the echo graph. E2 adds tool calls
(`TOOL_CALL_START/ARGS/END/RESULT`), shared state (`STATE_SNAPSHOT/DELTA`),
and the real model — behind the same envelope, so clients written against
E1 keep working.

## LangGraph agents

`app/graph.py` owns the graphs. The E1 graph is `START → echo → END`, a
real compiled `StateGraph` (not a mock): the endpoint consumes graphs only
through `run_stream()`, so E2's personal-AI agent — Sarvam-30B, PG-backed
conversation memory per user thread, system prompt from versioned YAML —
slots in without touching the HTTP layer.

## Model runtime

| Env | Runtime | Notes |
|-----|---------|-------|
| dev (compose) | Ollama (`ollama/ollama`), model `AI_MODEL` (default `qwen3:8b`) | Pull once: `docker compose exec ollama ollama pull qwen3:8b`. CPU-friendly; E1 doesn't call it yet (echo graph), E2 does. |
| prod | vLLM, OpenAI-compatible endpoint | Env-point the service at it in E2; noted here so the seam is designed, not bolted on. |

Both speak the OpenAI chat-completions shape, so the E2 model client stays
runtime-agnostic.

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `PORT` | `8000` | Listen port inside the container |
| `AI_SERVICE_TOKEN` | *(required)* | Shared secret with the Go proxy; empty → service refuses to boot |
| `OLLAMA_BASE_URL` | `http://ollama:11434` | Dev model runtime |
| `AI_MODEL` | `qwen3:8b` | Model name (E2 wires it in) |

Go side: `AI_SERVICE_URL` (default `http://ai:8000`), `AI_SERVICE_TOKEN`
(same value). Token set + unparseable URL → API refuses to boot. Token
empty → `/v1/ai/*` returns `503` (envelope code `service_unavailable`,
message "ai service not configured"); token set + AI down at request
time → `503` + `Retry-After: 5`, message "ai service unavailable".

## Wave-2 roadmap (what plugs in where)

- **E2 personal AI chat** — POST `/v1/ai/chat` (SSE) on the Go side; the
  LangGraph agent gains Sarvam-30B, PG conversation memory, versioned-YAML
  system prompt, per-user rate limit + daily token budget.
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
