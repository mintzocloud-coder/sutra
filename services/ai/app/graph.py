# SPDX-License-Identifier: AGPL-3.0-or-later
"""LangGraph agent graphs for the Sutra AI service.

E1 shipped the graph SHAPE with a single echo node: a real compiled
StateGraph that takes the last user message and streams it back. E2
adds the personal-AI agent — ``recall -> generate -> persist`` — with
PG-backed conversation memory per user thread, the versioned system
prompt, and real model streaming via LangGraph's custom stream mode.
Both graphs are consumed through streaming interfaces so the AG-UI
endpoint never touches graph internals.
"""
from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass
from typing import TypedDict

from langgraph.graph import END, START, StateGraph


class EchoState(TypedDict):
    """Minimal agent state: the input text and the produced reply."""

    input: str
    output: str


def echo_node(state: EchoState) -> dict:
    """The E1 agent: reflect the last user message back verbatim.

    Deliberately trivial — it proves the graph compiles, runs, and
    streams through the AG-UI endpoint before any model is wired in.
    """
    return {"output": state.get("input", "")}


def build_echo_graph():
    """Compile the single-node echo graph (START -> echo -> END)."""
    graph = StateGraph(EchoState)
    graph.add_node("echo", echo_node)
    graph.add_edge(START, "echo")
    graph.add_edge("echo", END)
    return graph.compile()


# The E1 agent instance. Kept for the /ag-ui/run skeleton endpoint.
agent = build_echo_graph()


def run_stream(user_text: str) -> Iterator[dict]:
    """Stream the E1 agent's node outputs for one user turn.

    Yields one dict per graph node execution, each shaped as
    ``{"node": <name>, "output": <text>}`` — the AG-UI endpoint
    translates these into TEXT_MESSAGE_* events.
    """
    for chunk in agent.stream({"input": user_text, "output": ""}):
        for node, values in chunk.items():
            output = values.get("output", "") if isinstance(values, dict) else ""
            yield {"node": node, "output": output}


# ---------------------------------------------------------------------------
# E2: personal-AI chat agent (recall -> generate -> persist)
# ---------------------------------------------------------------------------


class ChatState(TypedDict, total=False):
    """State for one personal-AI turn."""

    subject: str            # verified user id (from X-Sutra-Subject)
    thread_id: str          # server-side ai_threads.id (UUID string)
    client_thread_id: str   # client-facing thread key
    user_text: str          # this turn's user message
    system_prompt: str      # versioned persona text
    prompt_version: str     # version stamp persisted with the reply
    messages: list          # full model input: system + history + user
    reply: str              # assistant reply (accumulated)
    prompt_tokens: int
    completion_tokens: int


@dataclass
class ChatDeps:
    """Everything the chat graph nodes need. Injected (not global) so the
    graph is testable without a database or a model runtime."""

    pool: object            # psycopg_pool.ConnectionPool
    model: object           # model.ModelClient
    max_history: int


def _stream_writer():
    """LangGraph custom-stream writer, or a no-op when the node runs
    outside a streaming context (e.g. unit tests via .invoke())."""
    try:
        from langgraph.config import get_stream_writer

        return get_stream_writer()
    except Exception:
        class _Noop:
            def __call__(self, _payload):
                pass

        return _Noop()


def build_chat_graph(deps: ChatDeps):
    """Compile the personal-AI graph: recall -> generate -> persist.

    * recall: resolve the per-user thread + load recent history + attach
      the versioned system prompt.
    * generate: stream the model, forwarding each delta through the
      custom stream channel so the HTTP layer can emit SSE as tokens
      arrive (true streaming, not buffered).
    * persist: save the turn (user + assistant rows) and record the real
      token usage for the daily budget.
    """

    def recall(state: ChatState) -> dict:
        # Local imports keep graph.py importable without a DB driver.
        from . import memory as memory_mod

        thread = memory_mod.get_or_create_thread(
            deps.pool, state["subject"], state.get("client_thread_id"),
            first_user_text=state["user_text"],
        )
        history = memory_mod.load_history(deps.pool, thread.id, deps.max_history)
        messages = [{"role": "system", "content": state["system_prompt"]}]
        messages.extend(history)
        messages.append({"role": "user", "content": state["user_text"]})
        return {
            "thread_id": thread.id,
            "client_thread_id": thread.client_thread_id,
            "messages": messages,
        }

    def generate(state: ChatState) -> dict:
        writer = _stream_writer()
        chunks: list[str] = []
        for delta in deps.model.stream_text(state["messages"]):
            chunks.append(delta)
            writer({"delta": delta})
        usage = deps.model.last_usage()
        return {
            "reply": "".join(chunks),
            "prompt_tokens": usage.prompt_tokens,
            "completion_tokens": usage.completion_tokens,
        }

    def persist(state: ChatState) -> dict:
        from . import budget as budget_mod
        from . import memory as memory_mod

        memory_mod.save_turn(
            deps.pool,
            state["thread_id"],
            state["user_text"],
            state.get("reply", ""),
            state.get("prompt_tokens", 0),
            state.get("completion_tokens", 0),
            state.get("prompt_version", ""),
        )
        budget_mod.record_usage(
            deps.pool,
            state["subject"],
            state.get("prompt_tokens", 0),
            state.get("completion_tokens", 0),
        )
        return {}

    graph = StateGraph(ChatState)
    graph.add_node("recall", recall)
    graph.add_node("generate", generate)
    graph.add_node("persist", persist)
    graph.add_edge(START, "recall")
    graph.add_edge("recall", "generate")
    graph.add_edge("generate", "persist")
    graph.add_edge("persist", END)
    return graph.compile()
