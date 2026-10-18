# SPDX-License-Identifier: AGPL-3.0-or-later
"""E2 unit tests: pure logic + chat-graph wiring. No database, no model
runtime — the graph is exercised with a fake pool and the stub model.
DB-backed behavior (migrations, guards, memory round-trips) is covered by
the live service test in the verification step and by smoke.sh."""
from __future__ import annotations

import sys
from contextlib import contextmanager
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app import chat as chat_mod
from app import graph as graph_mod
from app import model as model_mod
from app import prompts as prompts_mod
from app import tokens as tokens_mod


# --- tokens ----------------------------------------------------------------

def test_estimate_tokens_basics():
    assert tokens_mod.estimate_tokens("") == 0
    assert tokens_mod.estimate_tokens("a") == 1
    assert tokens_mod.estimate_tokens("abcd") == 1
    assert tokens_mod.estimate_tokens("abcde") == 2
    # ~4 chars per token.
    assert tokens_mod.estimate_tokens("x" * 400) == 100


def test_estimate_messages_adds_framing():
    bare = tokens_mod.estimate_tokens("hello")
    with_overhead = tokens_mod.estimate_messages([{"role": "user", "content": "hello"}])
    assert with_overhead == bare + 4


# --- prompts ---------------------------------------------------------------

def test_load_system_prompt_v1():
    prompt = prompts_mod.load_system_prompt("v1")
    assert prompt.version == "v1"
    assert prompt.name
    assert "Sutra" in prompt.content
    assert len(prompt.content) > 200


def test_load_system_prompt_missing_version():
    with pytest.raises(FileNotFoundError):
        prompts_mod.load_system_prompt("does-not-exist")


# --- stub model ------------------------------------------------------------

def test_stub_client_streams_and_marks_itself():
    client = model_mod.StubClient()
    chunks = list(client.stream_text([{"role": "user", "content": "hello there"}]))
    reply = "".join(chunks)
    assert len(chunks) > 1  # genuinely streamed, not one blob
    assert "AI_MODEL_STUB=1" in reply
    assert "hello there" in reply
    usage = client.last_usage()
    assert usage.prompt_tokens > 0
    assert usage.completion_tokens > 0


def test_stub_client_no_user_message():
    client = model_mod.StubClient()
    reply = "".join(client.stream_text([{"role": "system", "content": "x"}]))
    assert "AI_MODEL_STUB=1" in reply


# --- chat validation --------------------------------------------------------

class _Settings:
    max_input_chars = 8000


def test_validate_rejects_empty():
    with pytest.raises(chat_mod.ChatError) as ei:
        chat_mod._validate("", [], _Settings())
    assert ei.value.status_code == 422


def test_validate_rejects_oversize():
    with pytest.raises(chat_mod.ChatError) as ei:
        chat_mod._validate("x" * 8001, [{"role": "user", "content": "x" * 8001}],
                          _Settings())
    assert ei.value.code == "message_too_long"


def test_last_user_text_picks_latest():
    msgs = [
        {"role": "user", "content": "first"},
        {"role": "assistant", "content": "reply"},
        {"role": "user", "content": "  "},
        {"role": "user", "content": "second"},
    ]
    assert chat_mod._last_user_text(msgs) == "second"
    assert chat_mod._last_user_text([{"role": "assistant", "content": "x"}]) == ""


def test_as_tuples_handles_both_shapes():
    assert list(chat_mod._as_tuples([("custom", {"delta": "x"})])) == [
        ("custom", {"delta": "x"})]
    assert list(chat_mod._as_tuples([{"recall": {}}])) == [
        ("updates", {"recall": {}})]


# --- chat graph wiring (fake pool, stub model) -------------------------------

class _FakeCursor:
    def __init__(self, pool):
        self.pool = pool

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def execute(self, sql, params=None):
        self.pool.statements.append((sql, params))
        sql_u = sql.strip().upper()
        if sql_u.startswith("INSERT INTO AI_THREADS"):
            self._row = ("uuid-thread-1", "test-subject",
                         params[1], params[2])
        elif sql_u.startswith("SELECT ROLE"):
            self._rows = [r for r in self.pool.messages]
        elif sql_u.startswith("SELECT ID FROM AI_THREADS"):
            self._row = None

    def fetchone(self):
        return getattr(self, "_row", None)

    def fetchall(self):
        return getattr(self, "_rows", [])


class _FakeConn:
    def __init__(self, pool):
        self.pool = pool

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def cursor(self):
        return _FakeCursor(self.pool)

    def commit(self):
        self.pool.commits += 1


class _FakePool:
    def __init__(self):
        self.statements = []
        self.commits = 0
        self.messages = []  # rows load_history returns

    @contextmanager
    def connection(self):
        yield _FakeConn(self)


def test_chat_graph_recall_generate_persist():
    pool = _FakePool()
    pool.messages = [("user", "earlier question"), ("assistant", "earlier answer")]
    deps = graph_mod.ChatDeps(pool=pool, model=model_mod.StubClient(), max_history=30)
    g = graph_mod.build_chat_graph(deps)
    out = g.invoke({
        "subject": "test-subject",
        "client_thread_id": "t1",
        "user_text": "hello",
        "system_prompt": "Be helpful.",
        "prompt_version": "v1",
    })
    # recall attached system + history + user
    roles = [m["role"] for m in out["messages"]]
    assert roles == ["system", "user", "assistant", "user"]
    assert out["messages"][0]["content"] == "Be helpful."
    # generate produced the stub reply with real token counts
    assert "AI_MODEL_STUB=1" in out["reply"]
    assert out["prompt_tokens"] > 0 and out["completion_tokens"] > 0
    # persist wrote the turn (one INSERT into ai_messages) in a transaction
    inserts = [s for s, _ in pool.statements if "INSERT INTO AI_MESSAGES" in s.upper()]
    assert len(inserts) == 1
    assert pool.commits >= 2  # thread upsert + save_turn (+ usage upsert)


def test_chat_graph_empty_history():
    pool = _FakePool()
    deps = graph_mod.ChatDeps(pool=pool, model=model_mod.StubClient(), max_history=30)
    g = graph_mod.build_chat_graph(deps)
    out = g.invoke({
        "subject": "s", "client_thread_id": "t", "user_text": "hi",
        "system_prompt": "sp", "prompt_version": "v1",
    })
    assert [m["role"] for m in out["messages"]] == ["system", "user"]
