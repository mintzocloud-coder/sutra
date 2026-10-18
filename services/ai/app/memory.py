# SPDX-License-Identifier: AGPL-3.0-or-later
"""PG-backed conversation memory for the Sutra AI service (E2).

Threads are per-user: (subject, client_thread_id). History loads
oldest-first with a LIMIT — never OFFSET. Each turn persists the user
message and the assistant reply in one transaction, stamped with the
system-prompt version that produced the reply.
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass


@dataclass
class Thread:
    id: str          # server-side UUID (ai_threads.id)
    subject: str
    client_thread_id: str
    title: str


def get_or_create_thread(pool, subject: str, client_thread_id: str | None,
                         first_user_text: str = "") -> Thread:
    """Fetch the caller's thread, creating it on first use. Thread ids are
    scoped to the subject: two users can never share (or guess) a thread."""
    if not client_thread_id:
        client_thread_id = f"thread-{uuid.uuid4().hex[:12]}"
    title = (first_user_text.strip()[:80] or "New chat")
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO ai_threads (subject, client_thread_id, title)
                VALUES (%s, %s, %s)
                ON CONFLICT (subject, client_thread_id)
                DO UPDATE SET updated_at = now()
                RETURNING id, subject, client_thread_id, title
                """,
                (subject, client_thread_id, title),
            )
            row = cur.fetchone()
        conn.commit()
    return Thread(id=str(row[0]), subject=row[1],
                  client_thread_id=row[2], title=row[3])


def load_history(pool, thread_id: str, limit: int) -> list[dict]:
    """Last ``limit`` messages of the thread, oldest first — the context
    window fed to the model. (A sliding-window memory: Wave 2 can add
    summarization here without changing the interface.)"""
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT role, content FROM (
                    SELECT role, content, created_at, id
                    FROM ai_messages
                    WHERE thread_id = %s AND role IN ('user', 'assistant')
                    ORDER BY created_at DESC, id DESC
                    LIMIT %s
                ) recent
                ORDER BY created_at ASC, id ASC
                """,
                (thread_id, limit),
            )
            return [{"role": role, "content": content}
                    for role, content in cur.fetchall()]


def save_turn(pool, thread_id: str, user_text: str, assistant_text: str,
              prompt_tokens: int, completion_tokens: int,
              prompt_version: str) -> None:
    """Persist one turn (user + assistant) atomically. The assistant row
    carries the token counts and the prompt version for budget accounting
    and prompt-rollout auditability."""
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO ai_messages
                    (thread_id, role, content, prompt_tokens,
                     completion_tokens, prompt_version)
                VALUES
                    (%s, 'user', %s, 0, 0, %s),
                    (%s, 'assistant', %s, %s, %s, %s)
                """,
                (thread_id, user_text, prompt_version,
                 thread_id, assistant_text, prompt_tokens,
                 completion_tokens, prompt_version),
            )
            cur.execute(
                "UPDATE ai_threads SET updated_at = now() WHERE id = %s",
                (thread_id,),
            )
        conn.commit()
