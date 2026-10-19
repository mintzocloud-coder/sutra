# SPDX-License-Identifier: AGPL-3.0-or-later
"""Read-only business data for the business copilot (H1).

The copilot answers catalog Q&A and order insights from the Go API's
commerce tables. It NEVER writes: every query block runs inside an
explicit ``BEGIN READ ONLY`` transaction, so a bug that tried to mutate
commerce state would die with Postgres error 25006
(read_only_sql_transaction) instead of corrupting data. The AI service
still owns only its ai_* tables (own migration ledger); these reads are
a scoped integration, documented in docs/business-ai.md.

Subject scoping: the caller is the verified OIDC subject forwarded by
the Go proxy (X-Sutra-Subject). It resolves subject -> users.id ->
sellers.user_id. A caller with no seller row is NOT a seller: catalog
Q&A and order insights answer 403 not_a_seller rather than leaking
another store's data.
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field


class BusinessError(Exception):
    """A business request that must fail as JSON (not SSE)."""

    def __init__(self, status_code: int, code: str, message: str,
                 retry_after: int | None = None):
        super().__init__(message)
        self.status_code = status_code
        self.code = code
        self.message = message
        self.retry_after = retry_after


@dataclass
class Seller:
    id: str
    store_name: str
    store_slug: str


@dataclass
class VariantFact:
    sku: str
    price_paise: int
    mrp_paise: int | None
    attributes: dict
    available: int


@dataclass
class ProductFact:
    id: str
    title: str
    description: str
    variants: list[VariantFact] = field(default_factory=list)


@dataclass
class InsightFacts:
    window_days: int
    status_counts: dict[str, int]
    delivered_revenue_paise: int
    top_products: list[dict]


def _readonly(pool, fn):
    """Run fn(cursor) inside BEGIN READ ONLY — write attempts fail loud."""
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute("BEGIN READ ONLY")
            try:
                return fn(cur)
            finally:
                cur.execute("ROLLBACK")


def resolve_seller(pool, subject: str) -> Seller:
    """The seller row for this OIDC subject, or raise 403 not_a_seller."""
    def _q(cur):
        cur.execute(
            """
            SELECT s.id, s.store_name, s.store_slug
            FROM sellers s
            JOIN users u ON u.id = s.user_id
            WHERE u.subject = %s
            """,
            (subject,),
        )
        return cur.fetchone()

    row = _readonly(pool, _q)
    if row is None:
        raise BusinessError(
            403, "not_a_seller",
            "this account has no seller store — business copilot is for sellers only")
    return Seller(id=str(row[0]), store_name=row[1], store_slug=row[2])


def catalog_lookup(pool, seller_id: str, keywords: list[str],
                   limit: int = 5) -> list[ProductFact]:
    """Active products of this seller matching ANY keyword, with their
    active variants (price, SKU, attributes, available stock).

    Keywords come from business.product_keywords() (content words with
    intent/stop words stripped); each is matched with ILIKE against
    title/description (substring, no wildcard injection — the pattern is
    fully escaped and every parameter is bound). Only this seller's
    rows, active only. Empty keywords -> [] (never dump the catalog).
    """
    keywords = [k for k in keywords if k][:6]
    if not keywords:
        return []

    def _q(cur):
        clauses = []
        params: list = [seller_id]
        for kw in keywords:
            like = "%" + kw.replace("\\", "\\\\").replace(
                "%", "\\%").replace("_", "\\_") + "%"
            clauses.append(
                "(title ILIKE %s ESCAPE '\\' OR description ILIKE %s ESCAPE '\\')")
            params.extend([like, like])
        params.append(limit)
        cur.execute(
            f"""
            SELECT id, title, description
            FROM products
            WHERE seller_id = %s AND status = 'active'
              AND ({' OR '.join(clauses)})
            ORDER BY updated_at DESC, id DESC
            LIMIT %s
            """,
            tuple(params),
        )
        return _fetch_products(cur, limit)

    return _readonly(pool, _q)


def catalog_recent(pool, seller_id: str, limit: int = 5) -> list[ProductFact]:
    """The seller's most recently updated active products (the fallback
    when a catalog question names no product keywords, e.g. "show my
    products"). Same variant hydration as catalog_lookup."""

    def _q(cur):
        cur.execute(
            """
            SELECT id, title, description
            FROM products
            WHERE seller_id = %s AND status = 'active'
            ORDER BY updated_at DESC, id DESC
            LIMIT %s
            """,
            (seller_id, limit),
        )
        return _fetch_products(cur, limit)

    return _readonly(pool, _q)


def _fetch_products(cur, limit: int) -> list[ProductFact]:
    """Hydrate ProductFacts (with active variants + available stock) from
    a cursor positioned after a products SELECT of (id, title, description)."""
    products = [
        ProductFact(id=str(pid), title=t, description=d)
        for pid, t, d in cur.fetchall()
    ]
    for p in products:
        cur.execute(
            """
            SELECT v.sku, v.price_paise, v.mrp_paise, v.attributes,
                   COALESCE(i.quantity, 0) - COALESCE(i.reserved, 0) AS available
            FROM product_variants v
            LEFT JOIN inventory i ON i.variant_id = v.id
            WHERE v.product_id = %s AND v.status = 'active'
            ORDER BY v.price_paise ASC, v.sku ASC
            """,
            (p.id,),
        )
        p.variants = [
            VariantFact(sku=sku, price_paise=price,
                        mrp_paise=mrp,
                        attributes=attrs if isinstance(attrs, dict)
                        else json.loads(attrs or "{}"),
                        available=max(int(avail), 0))
            for sku, price, mrp, attrs, avail in cur.fetchall()
        ]
    return products


def order_insights(pool, seller_id: str,
                   window_days: int = 30) -> InsightFacts:
    """Order dashboard facts for this seller over the trailing window:
    status counts, delivered-order revenue, top products by units sold.
    """
    def _q(cur):
        cur.execute(
            """
            SELECT status, COUNT(*)
            FROM orders
            WHERE seller_id = %s AND created_at >= now() - %s::interval
            GROUP BY status
            """,
            (seller_id, f"{window_days} days"),
        )
        status_counts = {status: int(n) for status, n in cur.fetchall()}

        cur.execute(
            """
            SELECT COALESCE(SUM(total_paise), 0)
            FROM orders
            WHERE seller_id = %s AND status = 'delivered'
              AND created_at >= now() - %s::interval
            """,
            (seller_id, f"{window_days} days"),
        )
        revenue = int(cur.fetchone()[0])

        cur.execute(
            """
            SELECT oi.title, SUM(oi.quantity) AS units,
                   SUM(oi.line_total_paise) AS revenue_paise
            FROM order_items oi
            JOIN orders o ON o.id = oi.order_id
            WHERE o.seller_id = %s
              AND o.status NOT IN ('cancelled')
              AND o.created_at >= now() - %s::interval
            GROUP BY oi.product_id, oi.title
            ORDER BY units DESC, revenue_paise DESC
            LIMIT 5
            """,
            (seller_id, f"{window_days} days"),
        )
        top = [
            {"title": title, "units": int(units),
             "revenue_paise": int(rev)}
            for title, units, rev in cur.fetchall()
        ]
        return InsightFacts(window_days=window_days,
                            status_counts=status_counts,
                            delivered_revenue_paise=revenue,
                            top_products=top)

    return _readonly(pool, _q)


def render_grounding_block(products: list[ProductFact],
                           insights: InsightFacts | None,
                           seller: Seller) -> str:
    """The verbatim fact block injected into the model context. The
    GroundingGuard (app/business.py) validates the reply against exactly
    these facts — renderer and guard must never drift apart, so this is
    the single source of both."""
    lines = [f"[STORE] {seller.store_name} (slug: {seller.store_slug})"]
    if products:
        lines.append("[CATALOG DATA]")
        for p in products:
            lines.append(f"- PRODUCT: {p.title} (id: {p.id})")
            if p.description:
                lines.append(f"  description: {p.description[:300]}")
            for v in p.variants:
                attrs = ", ".join(f"{k}={val}"
                                  for k, val in v.attributes.items()) or "no attributes"
                mrp = f", mrp ₹{_paise_to_rs(v.mrp_paise)}" if v.mrp_paise else ""
                lines.append(
                    f"  - SKU {v.sku}: price ₹{_paise_to_rs(v.price_paise)}"
                    f"{mrp}, {attrs}, stock available: {v.available}")
    else:
        lines.append("[CATALOG DATA] (no products matched this query)")
    if insights is not None:
        lines.append(f"[ORDER INSIGHTS] trailing {insights.window_days} days")
        if insights.status_counts:
            lines.append("  orders by status: " + ", ".join(
                f"{s}={n}" for s, n in sorted(insights.status_counts.items())))
        else:
            lines.append("  no orders in this window")
        lines.append(f"  delivered revenue: "
                     f"₹{_paise_to_rs(insights.delivered_revenue_paise)}")
        for t in insights.top_products:
            lines.append(f"  top product: {t['title']} — {t['units']} units, "
                         f"₹{_paise_to_rs(t['revenue_paise'])} revenue")
    return "\n".join(lines)


def _paise_to_rs(paise: int) -> str:
    return f"{paise / 100:,.2f}".rstrip("0").rstrip(".")
