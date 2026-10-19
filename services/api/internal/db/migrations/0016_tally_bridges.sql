-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Migration 0016: per-seller Tally bridge registry (H2 part 2b, Wave 5).
--
-- Tally Prime exposes its XML-over-HTTP API only on the merchant's own
-- machine, so there is NO platform-wide Tally connection: every seller
-- runs their own on-premise bridge agent (design in docs/integrations.md)
-- and registers its base URL here. The API layer resolves the bridge
-- per seller (internal/integrations TallyHandler.bridgeRegistered) —
-- never from a global config — so one seller's agent can never see
-- another seller's vouchers.
--
-- Bridge SECRETS are deliberately NOT stored here. Until the
-- post-investors onboarding, per-job signing is delivered through the
-- bridge-agent handshake (docs/integrations.md); committing shared
-- secrets to a ledger table would be a credential leak by design. The
-- table records registration + reachability only.
--
-- status lifecycle (enforced by CHECK + the Go layer):
--   pending    — seller registered a bridge URL; not yet verified
--   active     — a verified agent heartbeat was seen (Wave 2)
--   disabled   — seller or platform turned the bridge off
-- Wave 1 never moves rows past pending: the PostVoucher route fails
-- closed (503) for every status until a real bridge provider is wired.
CREATE TABLE seller_tally_bridges (
    seller_id     uuid        PRIMARY KEY REFERENCES sellers(id) ON DELETE CASCADE,
    bridge_url    text        NOT NULL
                              CHECK (bridge_url ~ '^https?://')
                              CHECK (char_length(bridge_url) <= 2000),
    status        text        NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'active', 'disabled')),
    registered_at timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS seller_tally_bridges_set_updated_at ON seller_tally_bridges;
CREATE TRIGGER seller_tally_bridges_set_updated_at
    BEFORE UPDATE ON seller_tally_bridges
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
