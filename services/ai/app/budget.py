# SPDX-License-Identifier: AGPL-3.0-or-later
"""Per-user guards for the Sutra AI service (E2): rate limit + token budget.

Both are PG-backed so they hold across replicas (unlike an in-memory
bucket). The rate limit is a fixed one-minute window; the budget is a
daily token cap on UTC days. Rejections carry a Retry-After in seconds so
clients back off instead of hammering.
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta, timezone


@dataclass
class GuardRejection(Exception):
    """Raised when a turn must not run. The HTTP layer maps these to 429."""
    code: str            # ai_rate_limited | ai_budget_exhausted
    message: str
    retry_after: int     # seconds


def _seconds_to_next_minute_utc() -> int:
    now = datetime.now(timezone.utc)
    nxt = (now + timedelta(minutes=1)).replace(second=0, microsecond=0)
    return max(1, int((nxt - now).total_seconds()))


def _seconds_to_utc_midnight() -> int:
    now = datetime.now(timezone.utc)
    nxt = (now + timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0)
    return max(1, int((nxt - now).total_seconds()))


def check_rate_limit(pool, subject: str, requests_per_minute: int) -> None:
    """Fixed-window per-user rate limit. The check itself increments the
    window: a rejected request still counts (standard fixed-window
    semantics — clients must back off on 429, not retry into it)."""
    if requests_per_minute <= 0:
        return
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO ai_rate_windows (subject, window_start, count)
                VALUES (%s, date_trunc('minute', now()), 1)
                ON CONFLICT (subject, window_start)
                DO UPDATE SET count = ai_rate_windows.count + 1
                RETURNING count
                """,
                (subject,),
            )
            count = cur.fetchone()[0]
            # Best-effort hygiene: drop windows older than 5 minutes so the
            # table never grows unboundedly.
            cur.execute(
                "DELETE FROM ai_rate_windows WHERE window_start < now() - interval '5 minutes'"
            )
        conn.commit()
    if count > requests_per_minute:
        raise GuardRejection(
            code="ai_rate_limited",
            message=f"personal AI rate limit exceeded ({requests_per_minute}/min)",
            retry_after=_seconds_to_next_minute_utc(),
        )


def check_daily_budget(pool, subject: str, estimated_tokens: int,
                       daily_budget: int) -> None:
    """Reject the turn up-front when today's spend plus the estimated cost
    of this turn would exceed the user's daily token budget. The estimate
    is conservative (chars/4 over-counts for most scripts), so a rejection
    means the budget is genuinely exhausted."""
    if daily_budget <= 0:
        return
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)
                FROM ai_usage_daily
                WHERE subject = %s AND day = (now() AT TIME ZONE 'utc')::date
                """,
                (subject,),
            )
            row = cur.fetchone()
            used = int(row[0]) if row else 0
    if used + estimated_tokens > daily_budget:
        raise GuardRejection(
            code="ai_budget_exhausted",
            message=f"daily AI token budget exhausted ({daily_budget} tokens/day)",
            retry_after=_seconds_to_utc_midnight(),
        )


def record_usage(pool, subject: str, prompt_tokens: int,
                 completion_tokens: int) -> None:
    """Add one turn's real token counts to today's usage row."""
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO ai_usage_daily (subject, day, prompt_tokens, completion_tokens)
                VALUES (%s, (now() AT TIME ZONE 'utc')::date, %s, %s)
                ON CONFLICT (subject, day)
                DO UPDATE SET
                    prompt_tokens = ai_usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
                    completion_tokens = ai_usage_daily.completion_tokens + EXCLUDED.completion_tokens
                """,
                (subject, prompt_tokens, completion_tokens),
            )
        conn.commit()
