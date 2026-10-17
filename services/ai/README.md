# services/ai — Python intelligence layer

**Status:** 🟡 Scaffold — Wave-2 research decided the components; implementation follows Wave-1.

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
