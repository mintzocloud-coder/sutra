-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0010: cart data model (F3, part 1: cart API).
--
-- Carts are per-user and hold live variant references — prices are always
-- read fresh from product_variants, so a cart never shows a stale price
-- (the immutable price snapshot is taken at checkout, when order_items
-- are written in F3 part 2). Cart lines survive listing state flips: if
-- a seller archives a variant and later reactivates it, the buyer's cart
-- restores. GET /v1/cart filters to buyable lines and reports the
-- unavailable ones as unavailable_count, so the client can say
-- "2 items are no longer available" instead of silently dropping them.
--
-- MAX LINE QUANTITY: 999 (sanity cap; the per-variant stock check in the
-- service is the real guard, quantity <= available).
--
-- Safe to re-run: IF NOT EXISTS throughout.

-- Carts: one row per user with items (the row is the cart's existence
-- marker; its columns are cart-level fields, starting with updated_at).
CREATE TABLE IF NOT EXISTS carts (
    user_id    uuid        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    updated_at timestamptz NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS carts_set_updated_at ON carts;
CREATE TRIGGER carts_set_updated_at
    BEFORE UPDATE ON carts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Cart items: one line per (user, variant). Quantity is the DESIRED
-- quantity; the service refuses adds that exceed available stock
-- (quantity - reserved on the inventory row) inside the same transaction
-- as the upsert, so two racing tabs can never over-book.
CREATE TABLE IF NOT EXISTS cart_items (
    user_id    uuid NOT NULL REFERENCES carts(user_id) ON DELETE CASCADE,
    variant_id uuid NOT NULL REFERENCES product_variants(id) ON DELETE CASCADE,
    quantity   int  NOT NULL,
    added_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT cart_items_qty_positive CHECK (quantity > 0),
    CONSTRAINT cart_items_qty_sane CHECK (quantity <= 999),
    CONSTRAINT cart_items_pkey PRIMARY KEY (user_id, variant_id)
);

-- Variant-side lookup: "which carts hold this variant" (used by future
-- seller-side tooling; checkout joins user-side).
CREATE INDEX IF NOT EXISTS cart_items_variant_idx ON cart_items (variant_id);
