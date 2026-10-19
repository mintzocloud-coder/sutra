// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// Tally voucher route (H2 part 2b): per-seller accounting via the
// on-premise bridge.
//
// Route (registered in cmd/api/main.go):
//   POST /v1/integrations/tally/vouchers   seller-scoped voucher post
//                                         (behind RequireAuth)
//
// Per-seller wiring: Tally Prime lives on the merchant's own machine,
// so the bridge is resolved PER SELLER from the seller_tally_bridges
// registry (migration 0016) — never from a global config. One seller's
// agent can never see another seller's vouchers. Bridge secrets are
// deliberately not stored in the registry (see the migration header);
// per-job signing is a Wave 2 handshake (docs/integrations.md).
//
// Fail-closed ladder (all honest, all 503 + Retry-After so the client
// retries instead of assuming the voucher landed):
//   1. caller owns no seller row        → 403 not_a_seller
//   2. seller has no bridge registered   → 503 tally_not_registered
//   3. bridge registered but the only wired provider is the stub
//      (until the post-investors deployment) → 503 tally_unavailable
//   4. a real bridge errors on the wire → 502 tally_upstream
//
// Voucher grammar is validated up front (kind, calendar-valid YYYYMMDD,
// non-empty ReferenceNo, balanced debit/credit lines, per-line caps) so
// a future real bridge rejects nothing the stub accepted — and so a
// bookkeeping error is caught at the API instead of corrupting a
// merchant's Tally books.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// maxTallyVoucherBody caps the voucher route's request body; a 100-line
// voucher is a few KiB, so 64 KiB keeps a malicious client from pinning
// a handler goroutine on a giant decode.
const maxTallyVoucherBody = 64 << 10

// maxTallyLines caps a voucher at 100 ledger lines — real merchant
// vouchers rarely exceed a dozen; this is a generosity ceiling, not a
// Tally limit.
const maxTallyLines = 100

// maxTallyLedgerRunes caps ledger display names at Tally's own limit.
const maxTallyLedgerRunes = 128

// maxTallyReferenceRunes caps the ReferenceNo (our order id / dedupe
// key) — it must fit Tally's reference field.
const maxTallyReferenceRunes = 128

// maxTallyNarrationRunes caps the narration line.
const maxTallyNarrationRunes = 1000

// maxTallyAmountPaise caps a single ledger line at ₹10 crore in paise —
// a sanity ceiling against a fat-fingered amount corrupting books.
const maxTallyAmountPaise = int64(10_00_00_00_000)

// TallyVoucherRequest is the route body. Amounts are integer paise
// (same convention as internal/catalog); positive = debit, negative =
// credit, and the lines must balance to zero.
type TallyVoucherRequest struct {
	Kind        string `json:"kind"`
	Date        string `json:"date"`
	ReferenceNo string `json:"reference_no"`
	Narration   string `json:"narration,omitempty"`
	Lines       []struct {
		LedgerName  string `json:"ledger_name"`
		AmountPaise int64  `json:"amount_paise"`
	} `json:"lines"`
}

// validateTallyVoucher enforces the voucher grammar before any bridge
// call; it returns the envelope code for the first violation. The
// balance rule is real accounting: Tally rejects unbalanced vouchers,
// so the API rejects them first, with a message the seller can act on.
func validateTallyVoucher(req *TallyVoucherRequest) (code string, msg string) {
	switch TallyVoucherKind(req.Kind) {
	case TallyVoucherSales, TallyVoucherJournal:
	default:
		return "invalid_request", "kind must be \"sales\" or \"journal\""
	}
	if len(req.Date) != 8 {
		return "invalid_request", "date must be YYYYMMDD"
	}
	if _, err := time.Parse("20060102", req.Date); err != nil {
		return "invalid_request", "date is not a valid calendar date (YYYYMMDD)"
	}
	if ref := strings.TrimSpace(req.ReferenceNo); ref == "" {
		return "invalid_request", "reference_no is required (your order id — the dedupe key)"
	} else if utf8.RuneCountInString(ref) > maxTallyReferenceRunes {
		return "invalid_request", "reference_no exceeds 128 characters"
	}
	if utf8.RuneCountInString(req.Narration) > maxTallyNarrationRunes {
		return "invalid_request", "narration exceeds 1000 characters"
	}
	if len(req.Lines) == 0 {
		return "invalid_request", "lines must contain at least one ledger line"
	}
	if len(req.Lines) > maxTallyLines {
		return "invalid_request", "lines exceed 100 ledger lines"
	}
	var sum int64
	seen := make(map[string]bool, len(req.Lines))
	for i, l := range req.Lines {
		name := strings.TrimSpace(l.LedgerName)
		if name == "" {
			return "invalid_request", "lines[" + itoa(i) + "].ledger_name is required"
		}
		if utf8.RuneCountInString(name) > maxTallyLedgerRunes {
			return "invalid_request", "lines[" + itoa(i) + "].ledger_name exceeds 128 characters"
		}
		if seen[name] {
			return "invalid_request", "lines[" + itoa(i) + "]: duplicate ledger " + name + " — net the lines first"
		}
		seen[name] = true
		if l.AmountPaise == 0 {
			return "invalid_request", "lines[" + itoa(i) + "].amount_paise must be non-zero (positive = debit, negative = credit)"
		}
		if l.AmountPaise > maxTallyAmountPaise || l.AmountPaise < -maxTallyAmountPaise {
			return "invalid_request", "lines[" + itoa(i) + "].amount_paise exceeds the ₹10 crore per-line ceiling"
		}
		sum += l.AmountPaise
	}
	if sum != 0 {
		return "invalid_request", "voucher does not balance: debits and credits must net to zero"
	}
	return "", ""
}

// TallyHandler serves the voucher route for one API instance.
type TallyHandler struct {
	pool *db.Pool
	// bridge is the deployment-wide provider (the fail-closed stub
	// until the post-investors bridge-agent deployment).
	bridge TallyBridge
	log    *slog.Logger
	// sellerID resolves the OIDC subject to the seller row id the
	// caller owns (same seam as the WhatsApp handler).
	sellerID func(ctx context.Context, subject string) (string, error)
	// bridgeRegistered answers whether the seller registered a
	// bridge agent in seller_tally_bridges. Production queries the
	// real table; tests substitute a stub (no DB needed).
	bridgeRegistered func(ctx context.Context, sellerID string) (bool, error)
}

// NewTallyHandler builds the handler. bridge may be nil (tests, or a
// deployment that wired the registry before the bridge seam): the route
// then answers 503.
func NewTallyHandler(pool *db.Pool, bridge TallyBridge, log *slog.Logger) *TallyHandler {
	return &TallyHandler{
		pool:             pool,
		bridge:           bridge,
		log:              log,
		sellerID:         resolveSeller(pool),
		bridgeRegistered: lookupBridgeRegistered(pool),
	}
}

// PostVoucher serves POST /v1/integrations/tally/vouchers.
func (h *TallyHandler) PostVoucher() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := middleware.SubjectFromContext(r.Context())
		if subject == "" {
			middleware.WriteStatusError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		var req TallyVoucherRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, maxTallyVoucherBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_request", "request body must be valid JSON with known fields only")
			return
		}
		if code, msg := validateTallyVoucher(&req); code != "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, code, msg)
			return
		}
		sellerID, err := h.sellerID(r.Context(), subject)
		if err != nil {
			if errors.Is(err, errNoSellerRow) {
				middleware.WriteJSONError(w, r, http.StatusForbidden,
					"not_a_seller", "Tally vouchers are seller-scoped: register a seller account first")
				return
			}
			h.log.Error("tally: seller resolution failed", "err", err,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
			return
		}
		registered, err := h.bridgeRegistered(r.Context(), sellerID)
		if err != nil {
			h.log.Error("tally: bridge registry lookup failed", "err", err,
				"seller_id", sellerID, "request_id", middleware.FromContext(r.Context()))
			middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
			return
		}
		if !registered {
			h.notRegistered(w, r)
			return
		}
		if h.bridge == nil {
			h.unavailable(w, r)
			return
		}
		voucher := TallyVoucher{
			Kind:        TallyVoucherKind(req.Kind),
			Date:        req.Date,
			ReferenceNo: strings.TrimSpace(req.ReferenceNo),
			Narration:   req.Narration,
		}
		for _, l := range req.Lines {
			voucher.Lines = append(voucher.Lines, TallyLine{
				LedgerName:  strings.TrimSpace(l.LedgerName),
				AmountPaise: l.AmountPaise,
			})
		}
		jobID, err := h.bridge.PostVoucher(r.Context(), voucher)
		if err != nil {
			var dis *ErrIntegrationDisabled
			if errors.As(err, &dis) {
				h.unavailable(w, r)
				return
			}
			h.log.Error("tally: bridge post failed", "err", err,
				"seller_id", sellerID, "reference_no", voucher.ReferenceNo,
				"request_id", middleware.FromContext(r.Context()))
			middleware.WriteJSONError(w, r, http.StatusBadGateway,
				"tally_upstream", "the seller's Tally bridge agent failed to accept the voucher")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": jobID})
	})
}

// notRegistered answers when the seller never registered a bridge
// agent: 503 + Retry-After is honest — the voucher did NOT land, and
// the client should retry after onboarding, not assume success.
func (h *TallyHandler) notRegistered(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
		"tally_not_registered", "this seller has no Tally bridge agent registered — install the bridge agent and register it (see docs/integrations.md)")
}

// unavailable answers when a bridge is registered but the deployment
// has no real bridge provider wired (the fail-closed stub until the
// post-investors deployment).
func (h *TallyHandler) unavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
		"tally_unavailable", "Tally posting is disabled on this deployment — bridge-agent support is post-investors (see docs/integrations.md)")
}

// lookupBridgeRegistered is the production registry seam: one indexed
// PK lookup on seller_tally_bridges. The row's status is informational
// in Wave 1 (any row counts as registered); status enforcement
// (pending/active/disabled) is Wave 2 with agent heartbeats.
func lookupBridgeRegistered(pool *db.Pool) func(ctx context.Context, sellerID string) (bool, error) {
	return func(ctx context.Context, sellerID string) (bool, error) {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM seller_tally_bridges WHERE seller_id = $1::uuid)`,
			sellerID).Scan(&exists)
		return exists, err
	}
}
