# SPDX-License-Identifier: AGPL-3.0-or-later
"""Versioned system prompts for the Sutra AI service (E2).

The assistant persona is NEVER hardcoded: it lives in
``services/ai/prompts/system/{version}.yaml`` and is loaded once at
startup. A persona change is a new YAML file plus the AI_PROMPT_VERSION
knob — and every persisted assistant message records the prompt_version
that produced it, so behavior changes are auditable after the fact.
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import yaml

PROMPTS_DIR = Path(__file__).resolve().parent.parent / "prompts" / "system"


@dataclass(frozen=True)
class SystemPrompt:
    version: str
    name: str
    content: str


def load_system_prompt(version: str) -> SystemPrompt:
    """Load and validate the system prompt for a version. Raises
    FileNotFoundError / ValueError on a missing or malformed file —
    startup fails closed rather than running with a wrong persona."""
    path = PROMPTS_DIR / f"{version}.yaml"
    raw = path.read_text(encoding="utf-8")
    data = yaml.safe_load(raw)
    if not isinstance(data, dict):
        raise ValueError(f"system prompt {path.name}: top level must be a mapping")
    for field in ("version", "name", "content"):
        if not data.get(field) or not isinstance(data[field], str):
            raise ValueError(f"system prompt {path.name}: '{field}' must be a non-empty string")
    if data["version"] != version:
        raise ValueError(
            f"system prompt {path.name}: version field {data['version']!r} "
            f"does not match requested version {version!r}"
        )
    return SystemPrompt(version=data["version"], name=data["name"], content=data["content"].strip())
