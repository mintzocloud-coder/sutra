# SPDX-License-Identifier: AGPL-3.0-or-later
"""Business copilot for sellers (H1): catalog Q&A + order insights.

One turn through POST /business (the Go proxy serves it as
/v1/ai/business):

  validate -> resolve seller (403 if not a seller) -> per-user rate
    limit (429) -> daily token budget (429)
    -> graph: recall -> retrieve -> generate -> persist

The "never invent attributes" guardrail lives in THREE layers:

  1. PROMPT — prompts/system/business_v1.yaml states the rule and what
     to say when a fact is missing.
  2. CODE — GroundingGuard (below, pure and unit-tested): the generate
     node buffers the model reply, extracts every stated price and SKU,
     and requires each to appear in the injected grounding block
     (rendered by bizdata.render_grounding_block — the SINGLE source of
     facts for both injection and validation, so they cannot drift).
     A violation swaps the reply for a safe fallback instead of being
     streamed. Streaming only starts AFTER the reply is verified: a
     fabricated price must never reach the wire, even partially.
  3. EVAL — services/ai/eval/golden/v1_business.yaml carries grounded
     cases (real-model backend) plus stub plumbing cases.

Money facts are normalized to paise for comparison (₹1,299 == ₹1299.00);
SKU mentions are the tokens after a literal "SKU " (the renderer's own
convention), required as substrings of the fact block. Counts and titles
are prompt+eval enforced: mechanical guarding of bare numbers produces
false positives on ordinary prose ("top 5 products") without grounding
value, so the guard deliberately covers only price and SKU — the two
attribute types with machine-checkable identity.
"""
from __future__ import annotations

import json
import logging
import re
import uuid
from collections.abc import Iterator
from dataclasses import dataclass
from typing import TypedDict

from langgraph.graph import END, START, StateGraph

from . import bizdata as biz_mod
from . import budget as budget_mod
from . import memory as memory_mod
from . import model as model_mod
from . import prompts as prompts_mod
from . import tokens as tokens_mod
from .config import Settings

log = logging.getLogger("sutra-ai-business")


class BusinessState(TypedDict, total=False):
    """State for one business-copilot turn."""

    subject: str            # verified OIDC subject (from X-Sutra-Subject)
    thread_id: str          # server-side ai_threads.id (UUID string)
    client_thread_id: str   # client-facing thread key
    user_text: str          # this turn's user message
    system_prompt: str      # versioned business_v1 persona text
    prompt_version: str     # version stamp persisted with the reply
    seller: object          # bizdata.Seller (resolved eagerly, never in-graph)
    intents: set            # classify_intent() output
    messages: list          # history (system + grounding attached in generate)
    grounding: str          # the verified fact block
    reply: str              # assistant reply (post GroundingGuard)
    grounding_violations: int
    prompt_tokens: int
    completion_tokens: int

# ---------------------------------------------------------------------------
# Intent classification (pure)
# ---------------------------------------------------------------------------

_INSIGHT_WORDS = {
    "order", "orders", "revenue", "sales", "sold", "selling", "delivered",
    "shipped", "return", "returns", "returned", "earning", "earnings",
    "income", "dashboard", "status", "cancelled", "profit", "units",
}
_CATALOG_WORDS = {
    "price", "cost", "stock", "sku", "variant", "size", "color", "colour",
    "product", "products", "catalog", "catalogue", "inventory", "mrp",
    "discount", "item", "items",
}


def classify_intent(text: str) -> set[str]:
    """Which business data sources this question needs.

    Returns a subset of {"catalog", "insights"}; the empty set means the
    turn is conversational and needs no grounding block. Pure: keyword
    matching on word boundaries, deterministic, unit-tested.
    """
    words = set(re.findall(r"[a-z]+", text.lower()))
    intents: set[str] = set()
    if words & _INSIGHT_WORDS:
        intents.add("insights")
    if words & _CATALOG_WORDS:
        intents.add("catalog")
    return intents


# Words that carry no product identity: question framing + the intent
# vocabularies above. Stripped before catalog keyword search so "what is
# the price of kurta?" searches for ["kurta"], not the whole sentence.
_CATALOG_STOPWORDS = {
    "what", "which", "whose", "whom", "this", "that", "these", "those",
    "there", "here", "the", "and", "for", "with", "from", "about",
    "tell", "show", "give", "list", "how", "much", "many", "does",
    "can", "you", "your", "yours", "are", "was", "were", "been",
    "have", "has", "had", "will", "would", "should", "could", "please",
    "kindly", "just", "like", "also", "very", "all", "any", "its",
}


def product_keywords(text: str) -> list[str]:
    """Content words usable as catalog search keywords.

    Pure and deterministic: lowercase alpha words (len >= 3), minus
    stopwords and the intent vocabularies, de-duplicated, capped at 6.
    "What is the price of red kurta?" -> ["red", "kurta"].
    """
    seen: list[str] = []
    for word in re.findall(r"[a-z]{3,}", text.lower()):
        if (word in _CATALOG_STOPWORDS or word in _INSIGHT_WORDS
                or word in _CATALOG_WORDS or word in seen):
            continue
        seen.append(word)
        if len(seen) >= 6:
            break
    return seen


# ---------------------------------------------------------------------------
# Grounding guard (pure)
# ---------------------------------------------------------------------------

_MONEY_RE = re.compile(
    r"(?:₹|Rs\.?|INR)\s*([\d,]+(?:\.\d{1,2})?)", re.IGNORECASE)
_SKU_MENTION_RE = re.compile(r"\bSKU\s+([A-Za-z0-9][A-Za-z0-9\-_./]{1,63})")


def _money_to_paise(amount: str) -> int:
    """'1,299.00' -> 129900. Malformed input -> 0 (never raises)."""
    try:
        whole, _, frac = amount.replace(",", "").partition(".")
        return int(whole or 0) * 100 + int((frac + "00")[:2])
    except ValueError:
        return 0


def extract_money_paise(text: str) -> set[int]:
    """All stated money amounts in paise (₹/Rs./INR prefixed)."""
    return {_money_to_paise(m) for m in _MONEY_RE.findall(text)} - {0}


def extract_sku_mentions(text: str) -> set[str]:
    """SKU identifiers the reply explicitly names ('SKU ABC-123').

    Trailing sentence punctuation is stripped: "SKU KURTA-RED-M." names
    the SKU KURTA-RED-M, and the guard must not flag punctuation as an
    invented attribute.
    """
    return {m.rstrip(".,;:)]}!?\"'")
            for m in _SKU_MENTION_RE.findall(text)} - {""}


@dataclass
class GroundingViolation:
    kind: str          # "price" | "sku"
    value: str         # the invented attribute, as written
    message: str


class GroundingGuard:
    """Validates a generated reply against the injected fact block.

    check() returns the violations (empty = grounded). enforce() returns
    (reply_to_serve, violations): a violating reply is replaced with the
    safe fallback — the client never sees the fabrication.
    """

    FALLBACK = (
        "I couldn't verify that detail against your catalog data, so I'm "
        "not going to state it — a wrong price or SKU would be worse than "
        "no answer. Ask me about a specific product shown in your data, "
        "or tell me which product you mean."
    )

    @staticmethod
    def check(reply: str, facts_text: str) -> list[GroundingViolation]:
        violations: list[GroundingViolation] = []
        fact_money = extract_money_paise(facts_text)
        for paise in extract_money_paise(reply):
            if paise not in fact_money:
                violations.append(GroundingViolation(
                    "price", f"{paise / 100:,.2f}",
                    f"price ₹{paise / 100:,.2f} not present in catalog data"))
        for sku in extract_sku_mentions(reply):
            if sku not in facts_text:
                violations.append(GroundingViolation(
                    "sku", sku,
                    f"SKU '{sku}' not present in catalog data"))
        return violations

    @classmethod
    def enforce(cls, reply: str,
                facts_text: str) -> tuple[str, list[GroundingViolation]]:
        violations = cls.check(reply, facts_text)
        if violations:
            return cls.FALLBACK, violations
        return reply, []


# ---------------------------------------------------------------------------
# LangGraph: recall -> retrieve -> generate -> persist
# ---------------------------------------------------------------------------


@dataclass
class BusinessDeps:
    pool: object
    model: object
    max_history: int
    max_grounding_chars: int


def build_business_graph(deps: BusinessDeps):
    """Compile the business-copilot graph.

    * recall: per-user thread + recent history (shared ai_threads table;
      the business_v1 prompt_version distinguishes these turns).
    * retrieve: catalog_lookup and/or order_insights per intent, rendered
      into the single grounding block (truncated at max_grounding_chars).
    * generate: model reply over system + grounding + history + user,
      buffered, then GroundingGuard.enforce() — only the verified reply
      is stored and later streamed. Violations are logged with the
      thread id for guardrail tuning.
    * persist: save the turn + record real token usage against the
      shared daily budget.
    """
    def recall(state: BusinessState) -> dict:
        thread = memory_mod.get_or_create_thread(
            deps.pool, state["subject"], state.get("client_thread_id"),
            first_user_text=state["user_text"],
        )
        history = memory_mod.load_history(
            deps.pool, thread.id, deps.max_history)
        return {
            "thread_id": thread.id,
            "client_thread_id": thread.client_thread_id,
            "messages": history,
        }

    def retrieve(state: BusinessState) -> dict:
        seller: biz_mod.Seller = state["seller"]
        intents: set = state.get("intents", set())
        products: list = []
        insights = None
        if "catalog" in intents:
            keywords = product_keywords(state["user_text"])
            if keywords:
                products = biz_mod.catalog_lookup(
                    deps.pool, seller.id, keywords)
            else:
                # "show my products" names nothing: list recent actives.
                products = biz_mod.catalog_recent(deps.pool, seller.id)
        if "insights" in intents:
            insights = biz_mod.order_insights(deps.pool, seller.id)
        block = biz_mod.render_grounding_block(products, insights, seller)
        if len(block) > deps.max_grounding_chars:
            block = (block[: deps.max_grounding_chars]
                     + "\n…(truncated: too much catalog data for one turn)")
        return {"grounding": block}

    def generate(state: BusinessState) -> dict:
        system = (state["system_prompt"]
                  + "\n\n=== FACTS FOR THIS TURN (ground every attribute in these) ===\n"
                  + state.get("grounding", ""))
        messages = ([{"role": "system", "content": system}]
                    + state.get("messages", [])
                    + [{"role": "user", "content": state["user_text"]}])
        raw = "".join(deps.model.stream_text(messages))
        usage = deps.model.last_usage()
        reply, violations = GroundingGuard.enforce(raw, state.get("grounding", ""))
        if violations:
            log.warning("business grounding violations (thread=%s, subject=%s): %s",
                        state.get("client_thread_id"), state.get("subject"),
                        "; ".join(v.message for v in violations))
        return {
            "reply": reply,
            "grounding_violations": len(violations),
            "prompt_tokens": usage.prompt_tokens,
            "completion_tokens": usage.completion_tokens,
        }

    def persist(state: BusinessState) -> dict:
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

    graph = StateGraph(BusinessState)
    graph.add_node("recall", recall)
    graph.add_node("retrieve", retrieve)
    graph.add_node("generate", generate)
    graph.add_node("persist", persist)
    graph.add_edge(START, "recall")
    graph.add_edge("recall", "retrieve")
    graph.add_edge("retrieve", "generate")
    graph.add_edge("generate", "persist")
    graph.add_edge("persist", END)
    return graph.compile()


# ---------------------------------------------------------------------------
# Service: eager guards + verified-reply SSE
# ---------------------------------------------------------------------------

# The pre-flight budget estimate assumes the grounding block; real blocks
# are usually smaller (5 products, capped above), so this over-reserves
# rather than under-guards.
_GROUNDING_ESTIMATE_CHARS = 6000


@dataclass
class BusinessService:
    pool: object
    settings: Settings
    prompt: prompts_mod.SystemPrompt
    model: model_mod.ModelClient
    biz_graph: object

    def stream_turn(self, subject: str, thread_id: str | None,
                    messages: list[dict]) -> Iterator[str]:
        """Validate + resolve seller + guard eagerly (raising
        BusinessError), then return the SSE frame iterator. The reply is
        computed and grounding-verified BEFORE the first frame: the
        client can never see an unverified price, even partially."""
        user_text = _last_user_text(messages)
        _validate(user_text, messages, self.settings)

        # Seller resolution is eager: the graph never runs for non-sellers,
        # so no store's data is ever touched on their behalf.
        seller = biz_mod.resolve_seller(self.pool, subject)
        intents = classify_intent(user_text)

        try:
            budget_mod.check_rate_limit(
                self.pool, subject, self.settings.biz_requests_per_minute)
            prompt_messages = (
                [{"role": "system", "content": self.prompt.content}]
                + [{"role": "user", "content": user_text}]
            )
            estimate = (tokens_mod.estimate_messages(prompt_messages)
                        + tokens_mod.estimate_tokens(
                            "x" * _GROUNDING_ESTIMATE_CHARS))
            budget_mod.check_daily_budget(
                self.pool, subject, estimate, self.settings.daily_token_budget)
        except budget_mod.GuardRejection as rej:
            raise BusinessError(429, rej.code, rej.message,
                                rej.retry_after) from rej

        run_id = f"run-{uuid.uuid4().hex[:12]}"
        client_thread_id = thread_id or f"thread-{uuid.uuid4().hex[:12]}"
        message_id = f"msg-{uuid.uuid4().hex[:12]}"
        state_in = {
            "subject": subject,
            "client_thread_id": client_thread_id,
            "user_text": user_text,
            "system_prompt": self.prompt.content,
            "prompt_version": self.prompt.version,
            "seller": seller,
            "intents": intents,
        }
        graph = self.biz_graph

        def frames() -> Iterator[str]:
            yield _event("RUN_STARTED", threadId=client_thread_id, runId=run_id)
            reply = ""
            try:
                for chunk in graph.stream(state_in, stream_mode="updates"):
                    for node in chunk:
                        yield _event("STEP_STARTED", stepName=node)
                        yield _event("STEP_FINISHED", stepName=node)
                        if node == "generate":
                            reply = chunk[node].get("reply", "")
                # Verified by GroundingGuard inside the graph before we get
                # here: stream the reply in chunks (AG-UI shape preserved,
                # no unverified token ever hits the wire).
                yield _event("TEXT_MESSAGE_START", messageId=message_id,
                             role="assistant")
                for i in range(0, len(reply), 400):
                    yield _event("TEXT_MESSAGE_CONTENT",
                                 messageId=message_id, delta=reply[i:i + 400])
                yield _event("TEXT_MESSAGE_END", messageId=message_id)
                yield _event("RUN_FINISHED", threadId=client_thread_id,
                             runId=run_id)
            except Exception as exc:  # never leave the client hanging
                yield _event("RUN_ERROR", message=str(exc)[:500])

        return frames()


def _event(event_type: str, **fields) -> str:
    payload = {"type": event_type, **fields}
    return f"data: {json.dumps(payload, separators=(',', ':'))}\n\n"


def _last_user_text(messages: list[dict]) -> str:
    for msg in reversed(messages):
        if msg.get("role") == "user" and str(msg.get("content", "")).strip():
            return str(msg["content"])
    return ""


def _validate(user_text: str, messages: list[dict], settings: Settings) -> None:
    if not isinstance(messages, list) or not messages:
        raise BusinessError(422, "invalid_message",
                            "messages must be a non-empty list")
    if len(messages) > 100:
        raise BusinessError(422, "invalid_message", "messages exceeds 100 entries")
    if not user_text:
        raise BusinessError(422, "invalid_message",
                            "no user message with non-empty content found")
    if len(user_text) > settings.max_input_chars:
        raise BusinessError(
            422, "message_too_long",
            f"message exceeds {settings.max_input_chars} characters")


def new_service(pool, settings: Settings) -> BusinessService:
    prompt = prompts_mod.load_system_prompt(settings.biz_prompt_version)
    client = model_mod.new_client(
        settings.model_base_url, settings.model, settings.model_stub)
    deps = BusinessDeps(pool=pool, model=client,
                        max_history=settings.biz_max_history,
                        max_grounding_chars=settings.biz_max_grounding_chars)
    return BusinessService(pool=pool, settings=settings, prompt=prompt,
                           model=client, biz_graph=build_business_graph(deps))
