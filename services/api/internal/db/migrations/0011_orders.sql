-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0011: orders + order_items (F3, part 2a: checkout + state machine).
--
-- COD-first: orders.payment_method is 'cod' for now; prepaid methods
-- arrive with the F4 payment adapter. Payment-aggregator onboarding is
-- post-investors (fail-closed, documented in docs/payments.md).
--
-- PRICE SNAPSHOT: order_items carries the immutable per-unit price
-- (price_paise / mrp_paise) copied from product_variants at checkout.
-- The cart reads prices live, so a seller repricing between cart-add and
-- checkout is settled at the checkout-time price — never a stale one.
--
-- STOCK RESERVATION: checkout re-verifies available stock
-- (inventory.quantity - inventory.reserved) per line inside the
-- checkout transaction and adds the ordered quantity to
-- inventory.reserved (row-locked FOR UPDATE). Transitions then:
--   - cancelled / returned: release the reservation
--     (reserved -= qty, quantity untouched)
--   - delivered: convert the reservation into a sale
--     (quantity -= qty, reserved -= qty)
-- reserved <= quantity is enforced by the inventory check constraint,
-- so a bug in release/convert arithmetic fails loudly at the DB.
--
-- MULTI-SELLER CARTS: a cart may mix sellers; checkout creates one
-- order per seller (the idiomatic marketplace split — each seller
-- fulfils independently). Orders are never merged afterwards.
--
-- Safe to re-run: IF NOT EXISTS throughout.

-- Orders: buyer + seller + lifecycle status. 'created' rows are
-- unconfirmed reservations; the state machine lives in internal/orders.
CREATE TABLE IF NOT EXISTS orders (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_id       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    seller_id      uuid        NOT NULL REFERENCES sellers(id) ON DELETE RESTRICT,
    status         text        NOT NULL DEFAULT 'created',
    payment_method text        NOT NULL DEFAULT 'cod',
    total_paise    bigint      NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_status_valid CHECK (status IN (
        'created', 'confirmed', 'shipped', 'delivered', 'cancelled', 'returned'
    )),
    CONSTRAINT orders_payment_method_valid CHECK (payment_method = 'cod'),
    CONSTRAINT orders_total_positive CHECK (total_paise > 0)
);

DROP TRIGGER IF EXISTS orders_set_updated_at ON orders;
CREATE TRIGGER orders_set_updated_at
    BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Order items: the immutable per-order snapshot. Variant/product
-- references use ON DELETE RESTRICT (not CASCADE): an order is a legal
-- record, and deleting a variant or product that has orders must fail
-- loudly rather than silently rewriting history.
CREATE TABLE IF NOT EXISTS order_items (
    order_id         uuid   NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    variant_id       uuid   NOT NULL REFERENCES product_variants(id) ON DELETE RESTRICT,
    product_id       uuid   NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    sku              text   NOT NULL,
    title            text   NOT NULL,
    price_paise      bigint NOT NULL,
    mrp_paise        bigint,
    quantity         int    NOT NULL,
    line_total_paise bigint NOT NULL,
    CONSTRAINT order_items_pkey PRIMARY KEY (order_id, variant_id),
    CONSTRAINT order_items_qty_positive CHECK (quantity > 0),
    CONSTRAINT order_items_price_positive CHECK (price_paise > 0),
    CONSTRAINT order_items_line_total CHECK (line_total_paise = price_paise * quantity)
);

-- Buyer view: "my orders, newest first".
CREATE INDEX IF NOT EXISTS orders_buyer_idx
    ON orders (buyer_id, created_at DESC, id DESC);

-- Seller view: "orders for my store, newest first".
CREATE INDEX IF NOT EXISTS orders_seller_idx
    ON orders (seller_id, created_at DESC, id DESC);

-- Dashboard slices: seller orders by status.
CREATE INDEX IF NOT EXISTS orders_seller_status_idx
    ON orders (seller_id, status, created_at DESC, id DESC);
