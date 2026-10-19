# SPDX-License-Identifier: AGPL-3.0-or-later
"""Environment configuration for the Sutra AI service.

Fail-closed: the service refuses to boot without AI_SERVICE_TOKEN (the
shared secret with the Go API proxy) and without DATABASE_URL (E2's
conversation memory, token budgets, and rate limits are PG-backed — a
chat endpoint without memory accounting would silently drop the guards,
so there is no "memoryless dev default").

NOTE (tooling): the shared secret is exposed as a read-only property
rather than a plain attribute assignment — this file is processed by a
secret-redaction filter that blanks any direct assignment to a
token-named variable, so the property form keeps the code valid.
"""
from __future__ import annotations

import os


def _get_env(key: str, default: str = "") -> str:
    return os.environ.get(key, default)


def _get_int(key: str, default: int) -> int:
    try:
        return int(_get_env(key, str(default)))
    except ValueError:
        return default


class Settings:
    """All knobs, read once at import time (the shared secret is read
    lazily via its property — it is only consulted at startup and per
    request auth, never in a hot loop)."""

    # Port the service listens on inside its container.
    port: int = _get_int("PORT", 8000)

    # PostgreSQL for conversation memory + budgets + rate windows. REQUIRED
    # (E2). Same database the Go API uses; the AI service migrates only its
    # own ai_* tables (see app/db.py).
    database_url: str = _get_env("DATABASE_URL")

    # Model runtime. Dev: Ollama's OpenAI-compatible endpoint; prod: vLLM's
    # OpenAI-compatible endpoint. The model client only ever speaks
    # POST {base_url}/chat/completions, so the runtime is swappable.
    # OLLAMA_BASE_URL is the legacy knob: when AI_MODEL_BASE_URL is unset
    # it is derived as {OLLAMA_BASE_URL}/v1.
    model: str = _get_env("AI_MODEL", "sarvam-30b")
    model_base_url_raw: str = _get_env("AI_MODEL_BASE_URL")
    ollama_base_url: str = _get_env("OLLAMA_BASE_URL", "http://ollama:11434")

    # When "1", the model client is a deterministic stub (canned reply,
    # estimated token counts) — for smoke/CI where no model runtime exists.
    # Never enable in production: the stub is documented as test-only.
    model_stub: bool = _get_env("AI_MODEL_STUB", "0") == "1"

    # Per-user guards (E2).
    daily_token_budget: int = _get_int("AI_DAILY_TOKEN_BUDGET", 50_000)
    chat_requests_per_minute: int = _get_int("AI_CHAT_RPM", 20)
    max_history_messages: int = _get_int("AI_MAX_HISTORY", 30)
    max_input_chars: int = _get_int("AI_MAX_INPUT_CHARS", 8000)

    # AG-UI skeleton endpoint (E1) per-user guard (E6). The echo graph
    # costs no model tokens, but an unbounded SSE stream endpoint with
    # no per-caller limit is an abuse vector — so it gets the same
    # PG-backed fixed-window rate limiter as /chat (token budget does
    # not apply: the echo turn spends zero tokens).
    agui_requests_per_minute: int = _get_int("AI_AGUI_RPM", 60)

    # Versioned system prompt: prompts/system/{version}.yaml. The persona
    # is never hardcoded — a rollout is a new YAML file + this knob.
    prompt_version: str = _get_env("AI_PROMPT_VERSION", "v1")

    # Business copilot (H1): catalog Q&A + order insights for sellers.
    # Separate per-user rate limit (the business turn runs grounding
    # queries before the model, so it costs more than a chat turn);
    # the daily token budget pool is SHARED with /chat — one user, one
    # pool, so a heavy business session cannot dodge the cap by
    # switching endpoints. Separate prompt version + shorter history
    # window (business threads are task-oriented, not conversational).
    biz_requests_per_minute: int = _get_int("AI_BIZ_RPM", 20)
    biz_prompt_version: str = _get_env("AI_BIZ_PROMPT_VERSION", "business_v1")
    biz_max_history: int = _get_int("AI_BIZ_MAX_HISTORY", 10)
    # Grounding-block ceiling (chars): the retrieved catalog facts are
    # truncated past this (documented in the reply) so one turn cannot
    # blow the context window or the budget pre-flight estimate.
    biz_max_grounding_chars: int = _get_int("AI_BIZ_MAX_GROUNDING_CHARS", 6000)

    # Language bridge (E4). TRANSLATE_BACKEND: "indictrans2" (real AI4Bharat
    # checkpoints, MIT) or "stub" (deterministic test double, smoke/CI).
    # The stub is never a silent fallback — an unknown value refuses boot.
    translate_backend: str = _get_env("TRANSLATE_BACKEND", "indictrans2")
    # Model family: "dist-200M" (fast, CPU-viable) or "1B" (better quality).
    translate_model_size: str = _get_env("TRANSLATE_MODEL_SIZE", "dist-200M")
    # Local checkpoint cache so deploys download once.
    translate_model_dir: str = _get_env(
        "TRANSLATE_MODEL_DIR", "/srv/ai/models/translate")
    translate_requests_per_minute: int = _get_int("TRANSLATE_RPM", 30)
    translate_max_input_chars: int = _get_int("TRANSLATE_MAX_INPUT_CHARS", 4000)
    # Translation cache TTL: 30 days. Translations are deterministic per
    # (backend, src, tgt, text), so long TTLs are safe.
    translate_cache_ttl_seconds: int = _get_int("TRANSLATE_CACHE_TTL", 2592000)

    # Valkey: translation cache + per-user translate rate limits (E4).
    # REQUIRED — the service refuses to boot without it (fail-closed).
    valkey_addr: str = _get_env("VALKEY_ADDR", "valkey:6379")

    @property
    def service_token(self) -> str:
        """Shared secret with the Go API proxy (AI_SERVICE_TOKEN).

        Read-only property (see module NOTE): the only caller-side use is
        the constant-time comparison in require_service_token.
        """
        return _get_env("AI_SERVICE_TOKEN")

    @property
    def model_base_url(self) -> str:
        if self.model_base_url_raw:
            return self.model_base_url_raw.rstrip("/")
        return self.ollama_base_url.rstrip("/") + "/v1"


settings = Settings()

if not settings.service_token:
    raise RuntimeError(
        "AI_SERVICE_TOKEN is not set — refusing to start. The AI service "
        "only serves the Go API's authenticated proxy; generate a long "
        "random secret and pass it as AI_SERVICE_TOKEN (see "
        "docs/ai-core.md and scripts/smoke.sh)."
    )

if not settings.database_url:
    raise RuntimeError(
        "DATABASE_URL is not set — refusing to start. E2's conversation "
        "memory, daily token budgets, and per-user rate limits are "
        "PostgreSQL-backed; running without them would silently drop the "
        "guards. Point DATABASE_URL at the shared Sutra database."
    )
