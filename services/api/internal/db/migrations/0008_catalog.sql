-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0008: commerce catalog data model (F1, part 1: schema).
--
-- Wave-3 commerce tables: sellers, categories, products,
-- product_variants, inventory.
--
-- 0%-COMMISSION PRICE MODEL: sellers.commission_rate_bps defaults to 0 —
-- the marketplace takes no cut of the sale price (Meesho's model, better
-- than the 5-30% take-rate of legacy marketplaces). The column is
-- per-seller and expressed in basis points (100 = 1%) so a future
-- differential rate needs no schema change. The platform monetizes via
-- advertising and logistics (later F-waves), never by skimming the
-- seller's price. Price lives on the VARIANT (price_paise), never on the
-- product: SKUs in different sizes/colors legitimately differ in price.
--
-- MONEY RULE: all prices are INTEGER paise (bigint). Floats never touch
-- money — no rounding drift, ever. mrp_paise is the optional reference
-- price for discount display; when present it must be >= price.
--
-- INVENTORY: quantity is stock on hand; reserved is stock held by
-- pending (unconfirmed) orders. available = quantity - reserved. The
-- CHECK(reserved <= quantity) is the DB backstop; the F6 order saga is
-- the writer that moves quantity <-> reserved atomically.
--
-- Safe to re-run: IF NOT EXISTS throughout; category seed rows are
-- INSERT ... ON CONFLICT (slug) DO NOTHING so re-running the migration
-- never duplicates the tree.

-- Sellers: one seller account per user (Wave 1 decision — matches the
-- KYC reality that a GSTIN/PAN identifies one supplier). store_slug is
-- the public storefront handle, first-come.
CREATE TABLE IF NOT EXISTS sellers (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid        NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    store_name          text        NOT NULL,
    store_slug          text        NOT NULL UNIQUE,
    status              text        NOT NULL DEFAULT 'active',
    commission_rate_bps int         NOT NULL DEFAULT 0,
    gstin               text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sellers_store_name_nonblank CHECK (btrim(store_name) <> ''),
    CONSTRAINT sellers_store_name_maxlen CHECK (char_length(store_name) <= 80),
    CONSTRAINT sellers_slug_shape CHECK (store_slug ~ '^[a-z0-9-]{3,30}$'),
    CONSTRAINT sellers_status_valid CHECK (status IN ('active', 'suspended')),
    CONSTRAINT sellers_commission_sane CHECK (commission_rate_bps >= 0 AND commission_rate_bps <= 10000),
    CONSTRAINT sellers_gstin_shape CHECK (gstin IS NULL OR gstin ~ '^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][A-Z0-9]Z[A-Z0-9]$')
);

DROP TRIGGER IF EXISTS sellers_set_updated_at ON sellers;
CREATE TRIGGER sellers_set_updated_at
    BEFORE UPDATE ON sellers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Categories: curated seed tree. depth is denormalized at seed time
-- (seeds are fixed; admin-managed edits are a later wave).
CREATE TABLE IF NOT EXISTS categories (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text        NOT NULL UNIQUE,
    name       text        NOT NULL,
    parent_id  uuid        REFERENCES categories(id) ON DELETE SET NULL,
    depth      int         NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT categories_name_nonblank CHECK (btrim(name) <> ''),
    CONSTRAINT categories_depth_sane CHECK (depth >= 0 AND depth <= 4)
);

CREATE INDEX IF NOT EXISTS categories_parent_idx ON categories (parent_id);

-- Seed the Wave-1 catalog tree. Slugs are the stable API identifiers;
-- names are display labels (i18n is a later wave). ON CONFLICT makes
-- the seed idempotent.
INSERT INTO categories (slug, name, depth) VALUES
    ('electronics',      'Electronics',            0),
    ('fashion',          'Fashion',                0),
    ('home-kitchen',     'Home & Kitchen',         0),
    ('grocery',          'Grocery',                0),
    ('beauty',           'Beauty & Personal Care', 0),
    ('books',            'Books & Stationery',     0),
    ('sports',           'Sports & Fitness',       0),
    ('toys',             'Toys & Games',           0),
    ('health',           'Health & Wellness',      0),
    ('automotive',       'Automotive',             0)
ON CONFLICT (slug) DO NOTHING;

-- Second level: children of the roots above. The WITH lookups resolve
-- parent ids by slug so the seed stays readable and idempotent.
WITH parent AS (
    SELECT id, slug FROM categories WHERE slug IN
        ('electronics','fashion','home-kitchen','grocery','beauty','books','sports')
)
INSERT INTO categories (slug, name, parent_id, depth)
SELECT v.slug, v.name, parent.id, 1
FROM (VALUES
    ('mobiles',          'Mobiles & Accessories', 'electronics'),
    ('laptops',          'Laptops & Computers',   'electronics'),
    ('audio',            'Audio',                 'electronics'),
    ('mens-fashion',     'Men''s Fashion',         'fashion'),
    ('womens-fashion',   'Women''s Fashion',       'fashion'),
    ('kids-fashion',     'Kids'' Fashion',         'fashion'),
    ('furniture',        'Furniture',             'home-kitchen'),
    ('kitchenware',      'Kitchen & Dining',      'home-kitchen'),
    ('home-decor',       'Home Decor',            'home-kitchen'),
    ('staples',          'Staples & Packaged Food','grocery'),
    ('dairy',            'Dairy & Eggs',          'grocery'),
    ('skincare',         'Skin Care',             'beauty'),
    ('haircare',         'Hair Care',             'beauty'),
    ('textbooks',        'Textbooks',             'books'),
    ('fitness-gear',     'Fitness Equipment',     'sports'),
    ('outdoor',          'Outdoor Sports',        'sports')
) AS v(slug, name, parent_slug)
JOIN parent ON parent.slug = v.parent_slug
ON CONFLICT (slug) DO NOTHING;

-- Products: the sellable entity. Status lifecycle:
-- draft (seller editing, invisible) -> active (listed) -> archived.
-- F2 search only indexes active products.
CREATE TABLE IF NOT EXISTS products (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    seller_id   uuid        NOT NULL REFERENCES sellers(id) ON DELETE CASCADE,
    category_id uuid        REFERENCES categories(id) ON DELETE SET NULL,
    title       text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT 'draft',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT products_title_nonblank CHECK (btrim(title) <> ''),
    CONSTRAINT products_title_maxlen CHECK (char_length(title) <= 200),
    CONSTRAINT products_desc_maxlen CHECK (char_length(description) <= 5000),
    CONSTRAINT products_status_valid CHECK (status IN ('draft', 'active', 'archived'))
);

DROP TRIGGER IF EXISTS products_set_updated_at ON products;
CREATE TRIGGER products_set_updated_at
    BEFORE UPDATE ON products
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Seller storefront listing: newest first.
CREATE INDEX IF NOT EXISTS products_seller_list_idx
    ON products (seller_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS products_category_idx
    ON products (category_id) WHERE status = 'active';

-- Product variants: the purchasable SKU. attributes is a small jsonb
-- bag ({size, color}) — structured enough for variant pickers, free
-- enough for any category. sku is unique per product (the seller's own
-- SKU namespace, not global).
CREATE TABLE IF NOT EXISTS product_variants (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id  uuid        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    sku         text        NOT NULL,
    price_paise bigint      NOT NULL,
    mrp_paise   bigint,
    attributes  jsonb       NOT NULL DEFAULT '{}',
    status      text        NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT product_variants_sku_nonblank CHECK (btrim(sku) <> ''),
    CONSTRAINT product_variants_sku_maxlen CHECK (char_length(sku) <= 64),
    CONSTRAINT product_variants_price_positive CHECK (price_paise > 0),
    CONSTRAINT product_variants_price_sane CHECK (price_paise <= 100000000),  -- 1e8 paise = 10 lakh INR
    CONSTRAINT product_variants_mrp_sane CHECK (mrp_paise IS NULL OR (mrp_paise > 0 AND mrp_paise >= price_paise)),
    CONSTRAINT product_variants_status_valid CHECK (status IN ('active', 'archived')),
    CONSTRAINT product_variants_sku_unique UNIQUE (product_id, sku)
);

DROP TRIGGER IF EXISTS product_variants_set_updated_at ON product_variants;
CREATE TRIGGER product_variants_set_updated_at
    BEFORE UPDATE ON product_variants
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Inventory: one row per variant (1:1, variant_id is the PK).
-- quantity = stock on hand; reserved = held by pending orders.
CREATE TABLE IF NOT EXISTS inventory (
    variant_id  uuid        PRIMARY KEY REFERENCES product_variants(id) ON DELETE CASCADE,
    quantity    int         NOT NULL DEFAULT 0,
    reserved    int         NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT inventory_qty_sane CHECK (quantity >= 0),
    CONSTRAINT inventory_reserved_sane CHECK (reserved >= 0),
    CONSTRAINT inventory_reserved_within_stock CHECK (reserved <= quantity)
);

DROP TRIGGER IF EXISTS inventory_set_updated_at ON inventory;
CREATE TRIGGER inventory_set_updated_at
    BEFORE UPDATE ON inventory
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
