# SPDX-License-Identifier: AGPL-3.0-or-later
"""Token estimation for the Sutra AI service (E2).

We deliberately do NOT ship a tokenizer model file: for the daily budget
guard, a fast heuristic is the right tool — it must run before every
turn, including the pre-flight estimate where the completion doesn't
exist yet. When the model runtime reports real usage (OpenAI-compatible
``usage`` block), the real numbers are recorded instead; the estimate is
only the pre-flight bound and the stub/fallback path.

Heuristic: ~4 characters per token (the standard rule of thumb for
Latin/CJK-mixed text; Indic scripts tokenize denser, so this errs on the
generous side — the budget guard stays conservative).
"""
from __future__ import annotations

_CHARS_PER_TOKEN = 4


def estimate_tokens(text: str) -> int:
    """Heuristic token count for a string. Always >= 1 for non-empty text."""
    if not text:
        return 0
    return max(1, (len(text) + _CHARS_PER_TOKEN - 1) // _CHARS_PER_TOKEN)


def estimate_messages(messages: list[dict]) -> int:
    """Heuristic prompt-token estimate for a chat message list, including a
    small per-message framing overhead (role tags, separators)."""
    total = 0
    for msg in messages:
        total += estimate_tokens(str(msg.get("content", "")))
        total += 4  # role + framing overhead per message
    return total
