# ADR-006: Wave-2 module verdicts (AI core)

**Date:** 2026-10-17 · **Status:** Accepted (research: Oct 2026, fit-ranked, popularity ignored)

## Personal AI — LangGraph + AG-UI 1.0; Sarvam-30B default

LangGraph (Python, MIT) + LangGraph Store for agent + memory, exposed over **AG-UI 1.0** (spec stable Sep 2026). Serve with **vLLM** (prod) / **Ollama** (contributor laptops) — both OpenAI-compatible, migration is a base-URL change. **Sarvam-30B** (Apache-2.0, Indic-first, single-GPU class) default; Sarvam-105B for hard reasoning. RAG/memory vectors in pgvector; tools via MCP. Caveat: Sarvam Hinglish/code-mix quality needs independent eval.

## Creator studio — build thin, buy nothing

faster-whisper (MIT) captions + FFmpeg burn-in (~6–7s per 15s clip, CPU); Sarvam-30B for scripts/titles; best-time-to-post from Wave-1 impression logs; thumbnails v1 = keyframe heuristics (diffusion = v2).

## Language bridge — IndicTrans2 + faster-whisper + Indic-Parler-TTS

**IndicTrans2** (MIT, all 22 scheduled languages, distilled 320M): <100ms/msg on CPU → inline realtime chat translation + caption MT, Valkey-cached. ASR: faster-whisper. Dubbing voice: **Indic-Parler-TTS** (Apache-2.0). **Rejected:** SeamlessM4T/NLLB (CC-BY-NC). IndicF5 license unverified — launch gate.

## Recommendations — two-tower over pgvector, ONNX ranker

Retrieval: multi-embedding two-tower (Pinterest KDD'25 pattern) over pgvector HNSW. Ranking: multi-task MLP via ONNX Runtime, watch-time primary. Training loop: impression/action stream → point-in-time joins → nightly retrain → embeddings to pgvector. Cold start: graduated exposure gates + exploration. Defer Feast; defer generative retrieval to Wave-3.
