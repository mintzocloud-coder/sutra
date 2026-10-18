# SPDX-License-Identifier: AGPL-3.0-or-later
"""LangGraph agent graphs for the Sutra AI service.

E1 ships the graph SHAPE with a single echo node: a real compiled
StateGraph that takes the last user message and streams it back. E2
replaces the echo node with the personal-AI agent (Sarvam-30B, PG-backed
conversation memory) while keeping this module's interface — the AG-UI
endpoint below consumes graphs only through ``run_stream``.
"""
from __future__ import annotations

from collections.abc import Iterator
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


# The E1 agent instance. E2 swaps this for the personal-AI graph.
agent = build_echo_graph()


def run_stream(user_text: str) -> Iterator[dict]:
    """Stream the agent's node outputs for one user turn.

    Yields one dict per graph node execution, each shaped as
    ``{"node": <name>, "output": <text>}`` — the AG-UI endpoint
    translates these into TEXT_MESSAGE_* events.
    """
    for chunk in agent.stream({"input": user_text, "output": ""}):
        for node, values in chunk.items():
            output = values.get("output", "") if isinstance(values, dict) else ""
            yield {"node": node, "output": output}
