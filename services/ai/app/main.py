# SPDX-License-Identifier: AGPL-3.0-or-later
"""FastAPI entrypoint for the Sutra AI service (E1).

Routes:
  GET  /healthz    — liveness, no auth (container healthchecks).
  POST /ag-ui/run  — AG-UI protocol endpoint (skeleton): accepts a
                     RunAgentInput, streams AG-UI BaseEvents as SSE.
                     Requires the shared service Bearer token.

Event model follows the AG-UI 1.0 vocabulary (RUN_STARTED, STEP_STARTED /
STEP_FINISHED, TEXT_MESSAGE_START / TEXT_MESSAGE_CONTENT /
TEXT_MESSAGE_END, RUN_FINISHED, RUN_ERROR). E1 emits the chat-shaped
subset over the echo graph; E2 adds tool calls, state snapshots, and the
real model behind the same envelope.
"""
from __future__ import annotations

import json
import secrets
import uuid
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, status
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel, Field

from . import graph as agent_graph
from .config import settings

app = FastAPI(title="sutra-ai", version="0.1.0")


def _new_id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:12]}"


def _event(event_type: str, **fields: Any) -> str:
    """One SSE data frame carrying an AG-UI BaseEvent."""
    payload = {"type": event_type, **fields}
    return f"data: {json.dumps(payload, separators=(',', ':'))}\n\n"


def require_service_token(authorization: str | None = Header(default=None)) -> None:
    """The Go API proxy is the only allowed caller.

    Constant-time comparison; missing/wrong token -> 401 with the same
    envelope shape as the Go API's errors (code/message), so clients see
    one consistent contract through the proxy.
    """
    if not authorization or not authorization.startswith("Bearer "):
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"error": {"code": "unauthorized", "message": "missing bearer token"}},
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


@app.get("/healthz")
def healthz() -> dict:
    return {"status": "ok", "service": "sutra-ai"}


@app.post("/ag-ui/run")
def ag_ui_run(
    body: RunAgentInput, _: None = Depends(require_service_token)
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
    """
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
        # One worker: the graphs hold in-process state until E2 moves
        # conversation memory to PostgreSQL. Scale via replicas, not workers.
        workers=1,
    )


if __name__ == "__main__":
    main()
