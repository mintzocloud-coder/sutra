# SPDX-License-Identifier: AGPL-3.0-or-later
"""Personal-AI chat service (E2): guards + LangGraph turn orchestration.

One turn through POST /chat (served by the Go proxy as /v1/ai/chat):

  validate -> per-user rate limit (429) -> per-user daily token budget (429)
    -> recall (thread + history + system prompt)
    -> generate (model streams deltas -> SSE TEXT_MESSAGE_CONTENT)
    -> persist (turn saved, real usage recorded)
    -> RUN_FINISHED

``stream_turn`` is a REGULAR function (not a generator): the guards run
eagerly when it is called, so rejections raise ChatError as clean JSON
429s (with Retry-After) before any SSE starts. It returns the frame
iterator; the graph itself streams via LangGraph's ("updates", "custom")
modes — node transitions become STEP_STARTED/STEP_FINISHED, model deltas
become TEXT_MESSAGE_CONTENT.
"""
from __future__ import annotations

import json
import uuid
from collections.abc import Iterator
from dataclasses import dataclass

from . import budget as budget_mod
from . import graph as graph_mod
from . import memory as memory_mod
from . import model as model_mod
from . import prompts as prompts_mod
from . import tokens as tokens_mod
from .config import Settings


class ChatError(Exception):
    """A turn that must fail as JSON (not SSE)."""

    def __init__(self, status_code: int, code: str, message: str,
                 retry_after: int | None = None):
        super().__init__(message)
        self.status_code = status_code
        self.code = code
        self.message = message
        self.retry_after = retry_after


@dataclass
class ChatService:
    pool: object
    settings: Settings
    prompt: prompts_mod.SystemPrompt
    model: model_mod.ModelClient
    chat_graph: object

    def stream_turn(self, subject: str, thread_id: str | None,
                    messages: list[dict]) -> Iterator[str]:
        """Validate + guard eagerly (raising ChatError), then return the
        SSE frame iterator for the turn."""
        user_text = _last_user_text(messages)
        _validate(user_text, messages, self.settings)

        # Guards first: both raise GuardRejection -> mapped to 429 below.
        try:
            budget_mod.check_rate_limit(
                self.pool, subject, self.settings.chat_requests_per_minute)
            # Pre-flight estimate is conservative on purpose: a turn is
            # only admitted when the budget provably covers it.
            history: list[dict] = []
            if thread_id:
                known = _peek_thread_id(self.pool, subject, thread_id)
                if known:
                    history = memory_mod.load_history(
                        self.pool, known, self.settings.max_history_messages)
            prompt_messages = (
                [{"role": "system", "content": self.prompt.content}]
                + history
                + [{"role": "user", "content": user_text}]
            )
            budget_mod.check_daily_budget(
                self.pool, subject,
                tokens_mod.estimate_messages(prompt_messages),
                self.settings.daily_token_budget)
        except budget_mod.GuardRejection as rej:
            raise ChatError(429, rej.code, rej.message, rej.retry_after) from rej

        run_id = f"run-{uuid.uuid4().hex[:12]}"
        client_thread_id = thread_id or f"thread-{uuid.uuid4().hex[:12]}"
        message_id = f"msg-{uuid.uuid4().hex[:12]}"
        state_in = {
            "subject": subject,
            "client_thread_id": client_thread_id,
            "user_text": user_text,
            "system_prompt": self.prompt.content,
            "prompt_version": self.prompt.version,
        }
        graph = self.chat_graph

        def frames() -> Iterator[str]:
            yield _event("RUN_STARTED", threadId=client_thread_id, runId=run_id)
            text_started = False
            try:
                stream = graph.stream(state_in, stream_mode=["updates", "custom"])
                for mode, payload in _as_tuples(stream):
                    if mode == "updates":
                        for node in payload:
                            yield _event("STEP_STARTED", stepName=node)
                            yield _event("STEP_FINISHED", stepName=node)
                    elif mode == "custom" and isinstance(payload, dict) \
                            and "delta" in payload:
                        if not text_started:
                            yield _event("TEXT_MESSAGE_START",
                                         messageId=message_id, role="assistant")
                            text_started = True
                        yield _event("TEXT_MESSAGE_CONTENT",
                                     messageId=message_id,
                                     delta=payload["delta"])
                if text_started:
                    yield _event("TEXT_MESSAGE_END", messageId=message_id)
                yield _event("RUN_FINISHED", threadId=client_thread_id,
                             runId=run_id)
            except Exception as exc:  # never leave the client hanging
                yield _event("RUN_ERROR", message=str(exc)[:500])

        return frames()


def _as_tuples(stream):
    """Normalize graph.stream() output to (mode, payload) tuples. With a
    list of stream modes LangGraph yields tuples; with a single mode it
    yields bare payloads — handle both defensively."""
    for chunk in stream:
        if isinstance(chunk, tuple) and len(chunk) == 2:
            yield chunk
        else:
            yield ("updates", chunk)


def _event(event_type: str, **fields) -> str:
    payload = {"type": event_type, **fields}
    return f"data: {json.dumps(payload, separators=(',', ':'))}\n\n"


def _last_user_text(messages: list[dict]) -> str:
    for msg in reversed(messages):
        if msg.get("role") == "user" and str(msg.get("content", "")).strip():
            return str(msg["content"])
    return ""


def _peek_thread_id(pool, subject: str, thread_id: str) -> str | None:
    """Thread UUID for the budget pre-flight estimate, without creating the
    thread (creation happens in the graph's recall node)."""
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT id FROM ai_threads WHERE subject = %s AND client_thread_id = %s",
                (subject, thread_id),
            )
            row = cur.fetchone()
            return str(row[0]) if row else None


def _validate(user_text: str, messages: list[dict], settings: Settings) -> None:
    if not isinstance(messages, list) or not messages:
        raise ChatError(422, "invalid_message", "messages must be a non-empty list")
    if len(messages) > 100:
        raise ChatError(422, "invalid_message", "messages exceeds 100 entries")
    if not user_text:
        raise ChatError(422, "invalid_message",
                        "no user message with non-empty content found")
    if len(user_text) > settings.max_input_chars:
        raise ChatError(
            422, "message_too_long",
            f"message exceeds {settings.max_input_chars} characters")


def new_service(pool, settings: Settings) -> ChatService:
    prompt = prompts_mod.load_system_prompt(settings.prompt_version)
    client = model_mod.new_client(
        settings.model_base_url, settings.model, settings.model_stub)
    deps = graph_mod.ChatDeps(pool=pool, model=client,
                              max_history=settings.max_history_messages)
    return ChatService(pool=pool, settings=settings, prompt=prompt,
                       model=client, chat_graph=graph_mod.build_chat_graph(deps))
