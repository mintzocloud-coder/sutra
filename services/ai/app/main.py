# SPDX-License-Identifier: AGPL-3.0-or-later
"""FastAPI entrypoint for the Sutra AI service.

Routes:
  GET  /healthz    — liveness, no auth (container healthchecks).
  POST /ag-ui/run  — AG-UI protocol endpoint (E1 skeleton): accepts a
                     RunAgentInput, streams AG-UI BaseEvents as SSE over the
                     echo graph. Requires the shared service Bearer <redacted>
  POST /chat       — E2 personal-AI chat (the Go proxy serves this as
                     /v1/ai/chat): LangGraph agent (recall -> generate ->
                     persist) with PG-backed per-user conversation memory,
                     versioned system prompt, per-user rate limit and daily
                     token budget. Streams AG-UI events as SSE. Requires the
                     service token AND X-Sutra-Subject (the verified user id
                     the Go proxy forwards; the AI service never sees JWTs).
  POST /translate  — E4 language bridge (the Go proxy serves this as
                     /v1/ai/translate): IndicTrans2 translation between en
                     and Indic languages, Valkey-cached by (hash, target),
                     per-user rate-limited. JSON response (not SSE).

Event model follows the AG-UI 1.0 vocabulary (RUN_STARTED, STEP_STARTED /
STEP_FINISHED, TEXT_MESSAGE_START / TEXT_MESSAGE_CONTENT /
TEXT_MESSAGE_END, RUN_FINISHED, RUN_ERROR).
"""
from __future__ import annotations

import asyncio
import json
import logging
import secrets
import uuid
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Request, status
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel, Field

from . import budget as budget_mod
from . import chat as chat_mod
from . import db as db_mod
from . import graph as agent_graph
from . import translate as translate_mod
from .config import settings

log = logging.getLogger("sutra-ai")


def _new_id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:12]}"


def _event(event_type: str, **fields: Any) -> str:
    """One SSE data frame carrying an AG-UI BaseEvent."""
    payload = {"type": event_type, **fields}
    return f"data: {json.dumps(payload, separators=(',', ':'))}\n\n"


def _envelope(status_code: int, code: str, message: str,
              retry_after: int | None = None) -> JSONResponse:
    headers = {"Retry-After": str(retry_after)} if retry_after else None
    return JSONResponse(
        status_code=status_code,
        content={"error": {"code": code, "message": message}},
        headers=headers,
    )


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Startup: run AI migrations (advisory-locked, idempotent), open the
    pool (fail-fast), load the versioned system prompt, build the chat
    graph. Any failure here refuses to boot — a chat service without its
    memory/budget tables or its persona must not serve traffic."""
    await asyncio.to_thread(db_mod.run_migrations, settings.database_url)
    pool = await asyncio.to_thread(db_mod.open_pool, settings.database_url)
    app.state.pool = pool
    app.state.chat = await asyncio.to_thread(chat_mod.new_service, pool, settings)
    app.state.translate = await asyncio.to_thread(
        translate_mod.new_service, settings)
    log.info("sutra-ai ready (prompt=%s model=%s stub=%s translate=%s)",
             settings.prompt_version, settings.model, settings.model_stub,
             settings.translate_backend)
    yield
    pool.close()


app = FastAPI(title="sutra-ai", version="0.2.0", lifespan=lifespan)


def require_service_token(authorization: str | None = Header(default=None)) -> None:
    """The Go API proxy is the only allowed caller.

    Constant-time comparison; missing/wrong token -> 401 with the same
    envelope shape as the Go API's errors (code/message), so clients see
    one consistent contract through the proxy.
    """
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"error": {"code": "unauthorized", "message": "missing bearer <redacted>"}},
        )
    presented = authorization[len("Bearer ") :]
    if not secrets.compare_digest(presented, settings.service_token):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"error": {"code": "unauthorized", "message": "invalid service token"}},
        )


class Message(BaseModel):
    role: str = Field(default="user")
    content: str = Field(default="")


class RunAgentInput(BaseModel):
    """AG-UI RunAgentInput (E1 subset): thread/run identity, the message
    list, and an opaque state bag the client owns."""

    thread_id: str = Field(default_factory=lambda: _new_id("thread"))
    run_id: str = Field(default_factory=lambda: _new_id("run"))
    messages: list[Message] = Field(default_factory=list)
    state: dict[str, Any] = Field(default_factory=dict)


class ChatInput(BaseModel):
    """E2 personal-AI chat input. thread_id is the client's conversation
    key (scoped to the user); omit it to start a new thread — the minted
    id comes back in RUN_STARTED's threadId."""

    thread_id: str | None = Field(default=None, max_length=128)
    messages: list[Message] = Field(default_factory=list)


class TranslateInput(BaseModel):
    """E4 language-bridge input. source="auto" resolves via Unicode
    script detection (ASCII -> en, Devanagari -> hi, ...); pass an
    explicit ISO code when the text mixes scripts or detection fails."""

    text: str = Field(default="")
    source: str = Field(default="auto")
    target: str = Field(default="en")


@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "service": "sutra-ai"}


@app.post("/ag-ui/run")
def ag_ui_run(
    body: RunAgentInput,
    request: Request,
    _: None = Depends(require_service_token),
) -> StreamingResponse:
    """Run one agent turn, streaming AG-UI events as SSE.

    E1 behavior: the last user message goes through the echo graph and
    comes back as a single assistant text message. The event sequence a
    client sees is:
      RUN_STARTED -> STEP_STARTED(echo) -> STEP_FINISHED(echo) ->
      TEXT_MESSAGE_START -> TEXT_MESSAGE_CONTENT -> TEXT_MESSAGE_END ->
      RUN_FINISHED
    Any graph failure mid-stream becomes RUN_ERROR (the terminal event)
    instead of a truncated stream.

    E6 abuse guards: the Go proxy authenticates the user (JWT) and
    forwards the verified subject as X-Sutra-Subject; without it the
    request is refused (the AI service must never attribute usage to an
    unknown caller). The per-user fixed-window rate limiter
    (AI_AGUI_RPM/min, default 60) runs eagerly before streaming:
    rejections are JSON 429s with Retry-After, never truncated streams.
    The token budget does not apply here — an echo turn spends zero
    model tokens.
    """
    subject = request.headers.get("x-sutra-subject", "").strip()
    if not subject:
        return _envelope(401, "unauthorized",
                         "missing X-Sutra-Subject — call through the Go API proxy")
    try:
        budget_mod.check_rate_limit(
            request.app.state.pool, subject, settings.agui_requests_per_minute)
    except budget_mod.GuardRejection as err:
        return _envelope(429, err.code, err.message, err.retry_after)
    user_text = ""
    for msg in reversed(body.messages):
        if msg.role == "user" and msg.content.strip():
            user_text = msg.content
            break

    message_id = _new_id("msg")
    thread_id, run_id = body.thread_id, body.run_id

    def event_stream():
        yield _event("RUN_STARTED", threadId=thread_id, runId=run_id)
        try:
            for out in agent_graph.run_stream(user_text):
                node = out["node"]
                yield _event("STEP_STARTED", stepName=node)
                yield _event("STEP_FINISHED", stepName=node)
                if out["output"]:
                    yield _event("TEXT_MESSAGE_START", messageId=message_id, role="assistant")
                    yield _event(
                        "TEXT_MESSAGE_CONTENT",
                        messageId=message_id,
                        delta=out["output"],
                    )
                    yield _event("TEXT_MESSAGE_END", messageId=message_id)
            yield _event("RUN_FINISHED", threadId=thread_id, runId=run_id)
        except Exception as exc:  # never leave the client hanging on a dead stream
            yield _event("RUN_ERROR", message=str(exc)[:500])

    return StreamingResponse(event_stream(), media_type="text/event-stream")


@app.post("/chat")
def chat(
    body: ChatInput,
    request: Request,
    _: None = Depends(require_service_token),
) -> StreamingResponse:
    """E2 personal-AI chat: one LangGraph turn, streamed as AG-UI SSE.

    The Go proxy authenticates the user (JWT) and forwards the verified
    subject as X-Sutra-Subject; without it the request is refused — the
    AI service must never attribute memory/budget to an unknown caller.
    Guards (validation, per-user rate limit, daily token budget) run
    before streaming: rejections are JSON 429s with Retry-After, never
    truncated streams.
    """
    subject = request.headers.get("x-sutra-subject", "").strip()
    if not subject:
        return _envelope(401, "unauthorized",
                         "missing X-Sutra-Subject — call through the Go API proxy")
    service: chat_mod.ChatService = request.app.state.chat
    try:
        frames = service.stream_turn(
            subject, body.thread_id,
            [{"role": m.role, "content": m.content} for m in body.messages],
        )
    except chat_mod.ChatError as err:
        return _envelope(err.status_code, err.code, err.message, err.retry_after)
    # Guards already passed (stream_turn raises eagerly on rejection), so
    # anything failing from here on becomes RUN_ERROR inside the SSE,
    # matching /ag-ui/run semantics.
    return StreamingResponse(frames, media_type="text/event-stream")


@app.post("/translate")
def translate(
    body: TranslateInput,
    request: Request,
    _: None = Depends(require_service_token),
) -> dict:
    """E4 language bridge: translate text between en and Indic languages.

    The Go proxy authenticates the user (JWT) and forwards the verified
    subject as X-Sutra-Subject; without it the request is refused. All
    guards (validation, per-user rate limit) run before the model:
    rejections are JSON error envelopes, never partial output. Response:
    {"text","source","target","cached","model"} — cached=true means the
    Valkey (hash, target) entry served this request with no model call.
    """
    subject = request.headers.get("x-sutra-subject", "").strip()
    if not subject:
        return _envelope(401, "unauthorized",
                         "missing X-Sutra-Subject — call through the Go API proxy")
    service: translate_mod.TranslationService = request.app.state.translate
    try:
        result = service.translate(subject, body.text, body.source, body.target)
    except translate_mod.TranslateError as err:
        return _envelope(err.status_code, err.code, err.message, err.retry_after)
    return {
        "text": result.text,
        "source": result.source,
        "target": result.target,
        "cached": result.cached,
        "model": result.model,
    }


@app.exception_handler(HTTPException)
async def _http_exception_handler(_request, exc: HTTPException) -> JSONResponse:
    # FastAPI's default handler would return {"detail": ...}; normalize to
    # the envelope our raise sites already build.
    detail = exc.detail
    if isinstance(detail, dict) and "error" in detail:
        return JSONResponse(status_code=exc.status_code, content=detail)
    request_id = _new_id("req")
    return JSONResponse(
        status_code=exc.status_code,
        content={
            "error": {
                "code": "error",
                "message": str(detail),
                "request_id": request_id,
            }
        },
    )


def main() -> None:
    import uvicorn

    uvicorn.run(
        "app.main:app",
        host="0.0.0.0",
        port=settings.port,
        log_level="info",
        # One worker: the pool + guards are process-local by design and
        # PG-backed, so replicas (not workers) are the scale unit.
        workers=1,
    )


if __name__ == "__main__":
    main()
