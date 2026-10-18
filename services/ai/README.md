# services/ai — Python intelligence layer

**Status:** 🟢 E2 personal-AI chat live — `POST /chat` (Go proxy:
`/v1/ai/chat`): LangGraph agent (`recall → generate → persist`) with
PG-backed per-user conversation memory, versioned-YAML system prompt
(`prompts/system/v1.yaml`), per-user rate limit + daily token budget
(both 429 with Retry-After), AG-UI 1.0 SSE streaming. Model: Sarvam-30B
default via Ollama dev / vLLM prod (OpenAI-compatible); `AI_MODEL_STUB=1`
for smoke/CI. E1's echo graph remains on `POST /ag-ui/run`.
See `docs/ai-core.md`. Tools, recsys, and the language bridge land in
E3–E5.

## What lives here (off the hot path — never in a request cycle)

- Personal AI agent (LangGraph + AG-UI 1.0, Sarvam-30B default via vLLM)
- Recommendations: two-tower retrieval (pgvector) + ONNX ranker + nightly training loop
- Language bridge: IndicTrans2 (chat/captions), faster-whisper (ASR), Indic-Parler-TTS (dubbing voice)
- Creator studio: captions, scripts, best-time-to-post
- Moderation pipelines

## Rules

- Python **never** serves user-facing requests directly — it consumes events / serves internal gRPC.
- All embeddings live in **PG18 + pgvector** (RAG, memory, user/video towers).
- Model licenses must be commercial-use-open (Apache-2.0/MIT). **Rejected:** anything CC-BY-NC (SeamlessM4T, NLLB).
- See `docs/architecture/ADR-006-wave2-ai.md`.
