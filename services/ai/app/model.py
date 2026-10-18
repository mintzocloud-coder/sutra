# SPDX-License-Identifier: AGPL-3.0-or-later
"""Model client for the Sutra AI service (E2).

One client, two runtimes: the client only speaks OpenAI-compatible
``POST {base_url}/chat/completions`` with ``stream: true``. In dev the
base URL points at Ollama's OpenAI-compat endpoint
(``http://ollama:11434/v1``); in prod it points at vLLM. The model name
is env-configurable (``AI_MODEL``, default ``sarvam-30b``).

``AI_MODEL_STUB=1`` swaps in a deterministic stub (canned reply,
estimated token counts) for smoke/CI, where no model runtime exists.
The stub is loudly test-only: it announces itself in every reply.
"""
from __future__ import annotations

import json
import logging
from dataclasses import dataclass

import httpx

from . import tokens

log = logging.getLogger("sutra-ai.model")


@dataclass
class Usage:
    prompt_tokens: int = 0
    completion_tokens: int = 0

    @property
    def total(self) -> int:
        return self.prompt_tokens + self.completion_tokens


class ModelError(Exception):
    """The model runtime failed (unreachable, 4xx/5xx, bad SSE)."""


class ModelClient:
    """Streams assistant deltas for a message list.

    ``stream_text`` yields text chunks; after the generator is exhausted,
    ``last_usage()`` reports token counts (real ``usage`` block when the
    runtime sent one, heuristic estimates otherwise).
    """

    def stream_text(self, messages: list[dict]):
        raise NotImplementedError

    def last_usage(self) -> Usage:
        raise NotImplementedError


class OpenAICompatClient(ModelClient):
    """OpenAI-compatible streaming chat client (Ollama /v1 or vLLM)."""

    def __init__(self, base_url: str, model: str, timeout: float = 120.0):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout
        self._usage = Usage()

    def stream_text(self, messages: list[dict]):
        url = f"{self.base_url}/chat/completions"
        payload = {
            "model": self.model,
            "messages": messages,
            "stream": True,
            # Ask for the usage block on the terminal chunk so budgets
            # are recorded from real counts, not estimates.
            "stream_options": {"include_usage": True},
        }
        try:
            with httpx.stream(
                "POST", url, json=payload, timeout=self.timeout
            ) as resp:
                if resp.status_code != 200:
                    raise ModelError(
                        f"model runtime returned HTTP {resp.status_code}"
                    )
                yield from self._parse_sse(resp, messages)
        except httpx.HTTPError as exc:
            raise ModelError(f"model runtime unreachable: {exc}") from exc

    def _parse_sse(self, resp, messages: list[dict]):
        full_text: list[str] = []
        for line in resp.iter_lines():
            if not line or not line.startswith("data:"):
                continue
            data = line[len("data:"):].strip()
            if data == "[DONE]":
                break
            try:
                chunk = json.loads(data)
            except json.JSONDecodeError:
                continue
            # Terminal usage chunk (no choices).
            usage = chunk.get("usage")
            if usage:
                self._usage = Usage(
                    prompt_tokens=int(usage.get("prompt_tokens", 0)),
                    completion_tokens=int(usage.get("completion_tokens", 0)),
                )
                continue
            for choice in chunk.get("choices", []):
                delta = choice.get("delta", {})
                text = delta.get("content")
                if text:
                    full_text.append(text)
                    yield text
        # Fallback when the runtime sent no usage block: estimate.
        if self._usage.total == 0:
            self._usage = Usage(
                prompt_tokens=tokens.estimate_messages(messages),
                completion_tokens=tokens.estimate_tokens("".join(full_text)),
            )

    def last_usage(self) -> Usage:
        return self._usage


class StubClient(ModelClient):
    """Deterministic test-only model: canned reply, estimated counts.

    Used by smoke/CI (AI_MODEL_STUB=1) where pulling a 30B model is
    impossible. Every reply is marked as a stub reply so a misconfigured
    production can never silently serve these to users.
    """

    def __init__(self):
        self._usage = Usage()

    def stream_text(self, messages: list[dict]):
        user_text = ""
        for msg in reversed(messages):
            if msg.get("role") == "user" and str(msg.get("content", "")).strip():
                user_text = str(msg["content"])
                break
        reply = (
            "[stub-model reply — AI_MODEL_STUB=1, no real model was called] "
            f"You said: {user_text[:200]}"
        )
        # Stream in a few chunks so the SSE path is genuinely exercised.
        for i in range(0, len(reply), 24):
            yield reply[i : i + 24]
        self._usage = Usage(
            prompt_tokens=tokens.estimate_messages(messages),
            completion_tokens=tokens.estimate_tokens(reply),
        )

    def last_usage(self) -> Usage:
        return self._usage


def new_client(base_url: str, model: str, stub: bool) -> ModelClient:
    if stub:
        log.warning("AI_MODEL_STUB=1 — serving deterministic stub replies (test-only)")
        return StubClient()
    return OpenAICompatClient(base_url, model)
