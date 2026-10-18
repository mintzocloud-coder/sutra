# SPDX-License-Identifier: AGPL-3.0-or-later
"""Sutra AI service — Python intelligence layer (E1 scaffold).

Serves the AG-UI protocol endpoint and the LangGraph agent graphs. It is
never reached directly by user clients: the Go API proxies /v1/ai/* to it
with a shared internal service token (see docs/ai-core.md).
"""
