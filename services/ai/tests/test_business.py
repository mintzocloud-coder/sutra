# SPDX-License-Identifier: AGPL-3.0-or-later
"""H1 unit tests: intent classification, keyword extraction, the
GroundingGuard, read-only data access, and the business-graph wiring.

No database, no model runtime — the graph is exercised with a fake
pool and the stub model (langgraph is installed in CI; these tests run
in the python-ai job). DB-backed behavior (migrations, guard 429s,
budget accounting) is covered by smoke.sh.
"""
from __future__ import annotations

import sys
from contextlib import contextmanager
from pathlib import Path
from types import SimpleNamespace

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app import bizdata as biz_mod
from app import business as business_mod
from app import model as model_mod
from app import prompts as prompts_mod

FACTS = """[STORE] Smoke Store (slug: smoke-store)
[CATALOG DATA]
- PRODUCT: Red Cotton Kurta (id: 11111111-1111-1111-1111-111111111111)
  description: Handloom cotton kurta
  - SKU KURTA-RED-M: price ₹1,299, mrp ₹1,999, size=M, color=red, stock available: 7
  - SKU KURTA-RED-L: price ₹1,349, size=L, color=red, stock available: 0
[ORDER INSIGHTS] trailing 30 days
  orders by status: delivered=12
  delivered revenue: ₹15,588
"""


# --- intent classification -------------------------------------------------

def test_classify_intent_catalog():
    assert business_mod.classify_intent("what is the price of kurta?") == {"catalog"}
    assert business_mod.classify_intent("show my products") == {"catalog"}


def test_classify_intent_insights():
    assert business_mod.classify_intent("how much revenue this month?") == {"insights"}
    assert business_mod.classify_intent("orders by status") == {"insights"}


def test_classify_intent_both():
    assert business_mod.classify_intent(
        "price of my top selling product") == {"catalog", "insights"}


def test_classify_intent_neither():
    assert business_mod.classify_intent("hello, how are you?") == set()
    assert business_mod.classify_intent("") == set()


# --- keyword extraction ----------------------------------------------------

def test_product_keywords_strips_framing():
    assert business_mod.product_keywords("What is the price of red kurta?") == [
        "red", "kurta"]


def test_product_keywords_empty_for_listing():
    # "show my products" names nothing -> the retrieve node falls back to
    # catalog_recent instead of a blind keyword search.
    assert business_mod.product_keywords("show my products") == []


def test_product_keywords_dedup_and_cap():
    kws = business_mod.product_keywords(
        "kurta kurta pajama shirt jeans top dress saree lehenga gown")
    assert kws == ["kurta", "pajama", "shirt", "jeans", "top", "dress"]


# --- money / SKU extraction -------------------------------------------------

def test_extract_money_paise_normalizes():
    assert business_mod.extract_money_paise("₹1,299") == {129900}
    assert business_mod.extract_money_paise("₹1299.00") == {129900}
    assert business_mod.extract_money_paise("Rs 500 and INR 1,000") == {50000, 100000}
    assert business_mod.extract_money_paise("no money here") == set()


def test_extract_sku_mentions():
    assert business_mod.extract_sku_mentions("order SKU KURTA-RED-M today") == {
        "KURTA-RED-M"}
    # "askew" must not match — the SKU keyword is required.
    assert business_mod.extract_sku_mentions("the plan is askew") == set()


# --- GroundingGuard ---------------------------------------------------------

def test_guard_clean_passes():
    reply = "The Red Cotton Kurta costs ₹1,299 (SKU KURTA-RED-M), 7 in stock."
    assert business_mod.GroundingGuard.check(reply, FACTS) == []


def test_guard_format_variants_match():
    # ₹1299.00 normalizes to the same paise as the fact's ₹1,299.
    reply = "Price is ₹1299.00 for SKU KURTA-RED-M."
    assert business_mod.GroundingGuard.check(reply, FACTS) == []


def test_guard_invented_price_flagged():
    reply = "The kurta costs ₹999, a steal!"
    violations = business_mod.GroundingGuard.check(reply, FACTS)
    assert len(violations) == 1
    assert violations[0].kind == "price"
    assert "999" in violations[0].value


def test_guard_invented_sku_flagged():
    reply = "Try SKU KURTA-BLUE-XL instead."
    violations = business_mod.GroundingGuard.check(reply, FACTS)
    assert len(violations) == 1
    assert violations[0].kind == "sku"
    assert violations[0].value == "KURTA-BLUE-XL"


def test_guard_small_talk_clean():
    assert business_mod.GroundingGuard.check(
        "Hello! How can I help with your store today?", FACTS) == []


def test_guard_enforce_swaps_fallback():
    reply, violations = business_mod.GroundingGuard.enforce(
        "It costs ₹999.", FACTS)
    assert violations
    assert reply == business_mod.GroundingGuard.FALLBACK
    assert "₹999" not in reply


def test_guard_enforce_keeps_clean_reply():
    reply, violations = business_mod.GroundingGuard.enforce(
        "It costs ₹1,299.", FACTS)
    assert violations == []
    assert reply == "It costs ₹1,299."


# --- read-only data access (fake pool) ----------------------------------------

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
        if sql_u.startswith("BEGIN READ ONLY"):
            self._rows, self._row = [], None
        elif sql_u.startswith("SELECT S.ID"):
            self._row = self.pool.seller_row
            self._rows = []
        elif "FROM PRODUCTS" in sql_u:
            self._rows = list(self.pool.products)
            self._row = None
        elif "FROM PRODUCT_VARIANTS" in sql_u:
            self._rows = list(self.pool.variants)
            self._row = None
        elif sql_u.startswith("SELECT STATUS, COUNT"):
            self._rows = list(self.pool.status_counts)
            self._row = None
        elif "SUM(TOTAL_PAISE)" in sql_u:
            self._row = (self.pool.revenue,)
            self._rows = []
        elif "SELECT OI.TITLE" in sql_u:
            self._rows = list(self.pool.top_products)
            self._row = None
        elif sql_u.startswith("INSERT INTO AI_THREADS"):
            self._row = ("uuid-biz-1", "seller-subject", params[1], params[2])
            self._rows = []
        elif sql_u.startswith("SELECT ROLE"):
            self._rows = list(self.pool.messages)
            self._row = None
        elif sql_u.startswith("INSERT INTO AI_RATE_WINDOWS"):
            self._row = (1,)
            self._rows = []
        elif sql_u.startswith("SELECT COALESCE(PROMPT_TOKENS"):
            self._row = (0,)
            self._rows = []
        else:
            self._rows, self._row = [], None

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
        self.messages = []
        self.seller_row = ("seller-uuid-1", "Smoke Store", "smoke-store")
        self.products = [("prod-uuid-1", "Red Cotton Kurta",
                           "Handloom cotton kurta")]
        self.variants = [("KURTA-RED-M", 129900, 199900,
                           {"size": "M", "color": "red"}, 7)]
        self.status_counts = [("delivered", 12)]
        self.revenue = 1558800
        self.top_products = [("Red Cotton Kurta", 12, 1558800)]

    @contextmanager
    def connection(self):
        yield _FakeConn(self)


def _statements_containing(pool, needle):
    return [s for s, _ in pool.statements if needle in s.upper()]


def test_readonly_transaction_wraps_catalog_lookup():
    pool = _FakePool()
    biz_mod.catalog_lookup(pool, "seller-uuid-1", ["kurta"])
    begins = _statements_containing(pool, "BEGIN READ ONLY")
    selects = _statements_containing(pool, "FROM PRODUCTS")
    assert begins, "catalog_lookup must open a READ ONLY transaction"
    # The BEGIN must precede the first products SELECT in statement order.
    idx_begin = next(i for i, (s, _) in enumerate(pool.statements)
                     if "BEGIN READ ONLY" in s.upper())
    idx_select = next(i for i, (s, _) in enumerate(pool.statements)
                      if "FROM PRODUCTS" in s.upper())
    assert idx_begin < idx_select
    assert selects


def test_resolve_seller_ok():
    pool = _FakePool()
    seller = biz_mod.resolve_seller(pool, "seller-subject")
    assert seller.store_name == "Smoke Store"
    assert seller.store_slug == "smoke-store"


def test_resolve_seller_not_a_seller():
    pool = _FakePool()
    pool.seller_row = None
    with pytest.raises(biz_mod.BusinessError) as exc:
        biz_mod.resolve_seller(pool, "random-user")
    assert exc.value.status_code == 403
    assert exc.value.code == "not_a_seller"


def test_catalog_lookup_empty_keywords_never_dumps():
    pool = _FakePool()
    assert biz_mod.catalog_lookup(pool, "seller-uuid-1", []) == []
    assert not _statements_containing(pool, "FROM PRODUCTS")


def test_render_grounding_block_is_guard_source():
    pool = _FakePool()
    seller = biz_mod.Seller(id="seller-uuid-1", store_name="Smoke Store",
                            store_slug="smoke-store")
    products = biz_mod.catalog_lookup(pool, "seller-uuid-1", ["kurta"])
    block = biz_mod.render_grounding_block(products, None, seller)
    assert "KURTA-RED-M" in block
    assert "₹1,299" in block
    # The guard validates against this exact block: a reply repeating its
    # facts is clean, anything else is flagged.
    assert business_mod.GroundingGuard.check(
        "SKU KURTA-RED-M costs ₹1,299.", block) == []


# --- graph wiring (fake pool, stub model) ------------------------------------

def _biz_settings():
    return SimpleNamespace(
        max_input_chars=8000,
        biz_requests_per_minute=20,
        daily_token_budget=50_000,
        biz_prompt_version="business_v1",
        biz_max_history=10,
        biz_max_grounding_chars=6000,
    )


def test_business_prompt_version_loads():
    prompt = prompts_mod.load_system_prompt("business_v1")
    assert prompt.version == "business_v1"
    assert "NEVER INVENT ATTRIBUTES" in prompt.content


def test_business_graph_grounding_flow():
    pool = _FakePool()
    deps = business_mod.BusinessDeps(
        pool=pool, model=model_mod.StubClient(), max_history=10,
        max_grounding_chars=6000)
    graph = business_mod.build_business_graph(deps)
    out = graph.invoke({
        "subject": "seller-subject",
        "client_thread_id": "t1",
        "user_text": "what is the price of kurta?",
        "system_prompt": "Be helpful.",
        "prompt_version": "business_v1",
        "seller": biz_mod.Seller(id="seller-uuid-1", store_name="Smoke Store",
                                 store_slug="smoke-store"),
        "intents": {"catalog"},
    })
    # retrieve injected the verified fact block
    assert "KURTA-RED-M" in out["grounding"]
    assert "₹1,299" in out["grounding"]
    # the stub echoes the user text (no money/SKU invented) -> clean reply
    assert "AI_MODEL_STUB=1" in out["reply"]
    assert out["grounding_violations"] == 0
    assert out["prompt_tokens"] > 0 and out["completion_tokens"] > 0
    # persist wrote the turn stamped with the business prompt version
    inserts = [(s, p) for s, p in pool.statements
               if "INSERT INTO AI_MESSAGES" in s.upper()]
    assert len(inserts) == 1
    assert "business_v1" in inserts[0][1]


def test_business_graph_violation_becomes_fallback():
    pool = _FakePool()
    deps = business_mod.BusinessDeps(
        pool=pool, model=model_mod.StubClient(), max_history=10,
        max_grounding_chars=6000)
    graph = business_mod.build_business_graph(deps)
    # The user asserts a wrong price; the stub echoes it; the guard must
    # replace the reply with the fallback instead of serving ₹999.
    out = graph.invoke({
        "subject": "seller-subject",
        "client_thread_id": "t1",
        "user_text": "is the kurta price ₹999?",
        "system_prompt": "Be helpful.",
        "prompt_version": "business_v1",
        "seller": biz_mod.Seller(id="seller-uuid-1", store_name="Smoke Store",
                                 store_slug="smoke-store"),
        "intents": {"catalog"},
    })
    assert out["grounding_violations"] == 1
    assert out["reply"] == business_mod.GroundingGuard.FALLBACK
    assert "₹999" not in out["reply"]


def test_service_stream_turn_sse_shape():
    pool = _FakePool()
    prompt = prompts_mod.load_system_prompt("business_v1")
    service = business_mod.BusinessService(
        pool=pool, settings=_biz_settings(), prompt=prompt,
        model=model_mod.StubClient(),
        biz_graph=business_mod.build_business_graph(
            business_mod.BusinessDeps(
                pool=pool, model=model_mod.StubClient(), max_history=10,
                max_grounding_chars=6000)))
    frames = list(service.stream_turn(
        "seller-subject", None,
        [{"role": "user", "content": "what is the price of kurta?"}]))
    import json as _json
    events = [_json.loads(f.split("data:", 1)[1]) for f in frames
              if f.startswith("data:")]
    types = [e["type"] for e in events]
    assert types[0] == "RUN_STARTED"
    assert types[-1] == "RUN_FINISHED"
    assert "STEP_STARTED" in types and "STEP_FINISHED" in types
    assert "TEXT_MESSAGE_START" in types and "TEXT_MESSAGE_END" in types
    text = "".join(e.get("delta", "") for e in events
                   if e["type"] == "TEXT_MESSAGE_CONTENT")
    assert text, "the verified reply must be streamed as content deltas"
    assert "AI_MODEL_STUB=1" in text


def test_service_stream_turn_not_a_seller():
    pool = _FakePool()
    pool.seller_row = None
    prompt = prompts_mod.load_system_prompt("business_v1")
    service = business_mod.BusinessService(
        pool=pool, settings=_biz_settings(), prompt=prompt,
        model=model_mod.StubClient(), biz_graph=None)
    with pytest.raises(biz_mod.BusinessError) as exc:
        service.stream_turn("random-user", None,
                            [{"role": "user", "content": "hi"}])
    assert exc.value.code == "not_a_seller"
