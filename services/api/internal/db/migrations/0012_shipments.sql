-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0012: shipments + COD remittance tracking fields (F5, part 2a).
--
-- COD-FIRST: orders.payment_method is 'cod' for now (migration 0011), so
-- every Wave-3 order ships with cash collected at the door. This table is
-- the ledger that reconciles what the courier collected against what was
-- expected — the money-side of fulfilment, kept in the DB (not a doc),
-- because a COD business runs on "who owes whom how much".
--
-- LIFECYCLE: status mirrors logistics.TrackingStatus wire values exactly
-- (created → picked_up → in_transit → out_for_delivery → delivered /
-- failed_delivery / cancelled / returned). The provider normalizes its
-- raw scans into these values; the DB CHECK keeps the set identical on
-- both sides of the seam. F6's saga consumes the transitions; the stub
-- provider never advances a shipment (it refuses every call).
--
-- REMITTANCE MODEL: a COD parcel has two money moments — (1) the courier
-- collects cod_expected_paise from the buyer at the door (what the door
-- handover reports into cod_collected_paise), and (2) the courier remits
-- the collected cash to the seller/aggregator later (remittance_status
-- pending → remitted, stamped in remitted_at). Shortfalls (collected <
-- expected) are a real operational case in Indian COD (part payments,
-- door refusals, rider mis-reporting); they are surfaced, not rounded:
-- cod_collected_paise NULL = "not reported yet", present-but-less-than-
-- expected = a shortfall the ops flow must chase. Prepaid orders (Wave 2+)
-- get remittance_status 'na' — nothing to remit.
--
-- Safe to re-run: IF NOT EXISTS throughout.

CREATE TABLE IF NOT EXISTS shipments (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id             uuid        NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    provider             text        NOT NULL DEFAULT 'stub',
    provider_shipment_id text,
    tracking_number      text,
    status               text        NOT NULL DEFAULT 'created',
    cod_expected_paise   bigint      NOT NULL DEFAULT 0,
    cod_collected_paise  bigint,
    remittance_status    text        NOT NULL DEFAULT 'pending',
    remitted_at          timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT shipments_status_valid CHECK (status IN (
        'created', 'picked_up', 'in_transit', 'out_for_delivery',
        'delivered', 'failed_delivery', 'cancelled', 'returned'
    )),
    CONSTRAINT shipments_remittance_valid CHECK (remittance_status IN (
        'pending', 'remitted', 'na'
    )),
    CONSTRAINT shipments_cod_expected_nonnegative CHECK (cod_expected_paise >= 0),
    CONSTRAINT shipments_cod_collected_nonnegative CHECK (
        cod_collected_paise IS NULL OR cod_collected_paise >= 0
    ),
    -- Remittance is a closed action: remitted rows must carry the stamp.
    CONSTRAINT shipments_remitted_stamped CHECK (
        remittance_status <> 'remitted' OR remitted_at IS NOT NULL
    )
);

CREATE INDEX IF NOT EXISTS shipments_order_idx ON shipments(order_id);
CREATE INDEX IF NOT EXISTS shipments_status_idx ON shipments(status);
CREATE INDEX IF NOT EXISTS shipments_remittance_idx ON shipments(remittance_status)
    WHERE remittance_status = 'pending';

DROP TRIGGER IF EXISTS shipments_set_updated_at ON shipments;
CREATE TRIGGER shipments_set_updated_at
    BEFORE UPDATE ON shipments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
