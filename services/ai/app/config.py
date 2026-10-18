# SPDX-License-Identifier: AGPL-3.0-or-later
"""Environment configuration for the Sutra AI service.

Fail-closed: the service refuses to boot without AI_SERVICE_TOKEN. The
token is the shared secret the Go API presents when proxying /v1/ai/*;
without it the service cannot distinguish the API from a stranger, so
there is no "dev default".
"""
from __future__ import annotations

import os


def _get_env(key: str, default: str = "") -> str:
    return os.environ.get(key, default)


class Settings:
    """All knobs, read once at import time."""

    # Port the service listens on inside its container.
    port: int = int(_get_env("PORT", "8000"))

    # Shared service-to-service secret with the Go API proxy. REQUIRED.
    service_token: str = _get_env("AI_SERVICE_TOKEN", "")

    # Model runtime. Dev: Ollama (see docker-compose.yml). Prod: vLLM —
    # the endpoint shape is OpenAI-compatible either way (docs/ai-core.md).
    ollama_base_url: str = _get_env("OLLAMA_BASE_URL", "http://ollama:11434")
    model: str = _get_env("AI_MODEL", "qwen3:8b")


settings = Settings()

if not settings.service_token:
    raise RuntimeError(
        "AI_SERVICE_TOKEN is not set — refusing to start. The AI service "
        "only serves the Go API's authenticated proxy; generate a long "
        "random secret and pass it as AI_SERVICE_TOKEN (see "
        "docs/ai-core.md and scripts/smoke.sh)."
    )
