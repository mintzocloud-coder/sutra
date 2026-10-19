// SPDX-License-Identifier: AGPL-3.0-or-later
package integrations

// Zoho Books routes (H2 final piece): customer contacts and invoices,
// seller-scoped, fail-closed until the post-investors Zoho OAuth
// onboarding.
//
// Routes (registered in cmd/api/main.go):
//   POST /v1/integrations/zoho/contacts   create a Zoho Books contact
//                                         (behind RequireAuth)
//   POST /v1/integrations/zoho/invoices    create a Zoho Books invoice
//                                         (behind RequireAuth)
//
// Per-seller scoping: both routes require the caller to own a seller
// row (403 not_a_seller otherwise) — a bookkeeping identity must never
// be creatable by a buyer. Zoho credentials are deployment-wide
// (one self-client per organization), so unlike Tally there is no
// per-seller registry: the fail-closed ladder is
//   1. caller owns no seller row        → 403 not_a_seller
//   2. the only wired provider is the stub
//      (until the post-investors onboarding) → 503 zoho_unavailable
//   3. a real provider errors on the wire → 502 zoho_upstream
// Every 503 carries Retry-After: the client retries after onboarding,
// never assumes an invoice was booked.
//
// Currency is pinned to INR (the house books everything in integer
// paise, INR-only until the international launch); the invoice grammar
// is validated up front so a future real provider rejects nothing the
// stub accepted — and so a bookkeeping error is caught at the API
// instead of corrupting the merchant's Zoho Books.

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

// maxZohoBody caps both Zoho routes' request bodies; a 100-line
// invoice is a few KiB, so 64 KiB keeps a malicious client from pinning
// a handler goroutine on a giant decode.
const maxZohoBody = 64 << 10

// maxZohoLines caps an invoice at 100 line items — real invoices
// rarely exceed a few dozen; this is a generosity ceiling, not a Zoho
// limit.
const maxZohoLines = 100

// maxZohoNameRunes caps display names and line-item names at Zoho's own
// display limits.
const maxZohoNameRunes = 128

// maxZohoReferenceRunes caps the ReferenceNo (our order id / dedupe
// key) — it must fit Zoho's reference-number field.
const maxZohoReferenceRunes = 128

// maxZohoContactIDRunes caps the Zoho contact id echoed back from the
// provider.
const maxZohoContactIDRunes = 64

// maxZohoQuantity caps a line-item quantity — a sanity ceiling against
// a fat-fingered quantity exploding the invoice total.
const maxZohoQuantity = 1000000

// maxZohoAmountPaise caps a single invoice line at ₹10 crore in paise —
// a sanity ceiling against a fat-fingered amount corrupting books.
const maxZohoAmountPaise = int64(10_00_00_00_000)

// ZohoContactRequest is the /zoho/contacts body.
type ZohoContactRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
}

// ZohoInvoiceRequest is the /zoho/invoices body. Amounts are integer
// paise (same convention as internal/catalog); positive only — an
// invoice line is a charge.
type ZohoInvoiceRequest struct {
	ContactID   string `json:"contact_id"`
	ReferenceNo string `json:"reference_no"`
	Currency    string `json:"currency"`
	DueDate     string `json:"due_date"` // YYYYMMDD
	Lines       []struct {
		Name        string `json:"name"`
		Quantity    int    `json:"quantity"`
		AmountPaise int64  `json:"amount_paise"`
	} `json:"lines"`
}

// validEmail is a deliberately weak sanity check — the route rejects
// obviously malformed addresses, Zoho does the real validation.
func validEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	dot := strings.LastIndexByte(email, '.')
	return dot > at+1 && dot < len(email)-1
}

// validateZohoContact enforces the contact grammar; it returns the
// envelope code for the first violation.
func validateZohoContact(req *ZohoContactRequest) (code string, msg string) {
	if name := strings.TrimSpace(req.DisplayName); name == "" {
		return "invalid_request", "display_name is required"
	} else if utf8.RuneCountInString(name) > maxZohoNameRunes {
		return "invalid_request", "display_name exceeds 128 characters"
	}
	if req.Email != "" && !validEmail(strings.TrimSpace(req.Email)) {
		return "invalid_request", "email is not a plausible address"
	}
	if req.Phone != "" {
		if err := ValidatePhone(req.Phone); err != nil {
			return "invalid_phone", "phone must be E.164 (+<8-15 digits>)"
		}
	}
	return "", ""
}

// validateZohoInvoice enforces the invoice grammar before any provider
// call; it returns the envelope code for the first violation. The
// invoice is charge-only (positive lines): credit notes are a Wave 2
// surface, and smuggling them in here would confuse the real Zoho
// provider's mapping.
func validateZohoInvoice(req *ZohoInvoiceRequest) (code string, msg string) {
	if id := strings.TrimSpace(req.ContactID); id == "" {
		return "invalid_request", "contact_id is required"
	} else if utf8.RuneCountInString(id) > maxZohoContactIDRunes {
		return "invalid_request", "contact_id exceeds 64 characters"
	}
	if ref := strings.TrimSpace(req.ReferenceNo); ref == "" {
		return "invalid_request", "reference_no is required (your order id — the dedupe key)"
	} else if utf8.RuneCountInString(ref) > maxZohoReferenceRunes {
		return "invalid_request", "reference_no exceeds 128 characters"
	}
	if req.Currency != "INR" {
		return "invalid_request", "currency must be \"INR\" (INR-only until the international launch)"
	}
	if len(req.DueDate) != 8 {
		return "invalid_request", "due_date must be YYYYMMDD"
	}
	if _, err := time.Parse("20060102", req.DueDate); err != nil {
		return "invalid_request", "due_date is not a valid calendar date (YYYYMMDD)"
	}
	if len(req.Lines) == 0 {
		return "invalid_request", "lines must contain at least one line item"
	}
	if len(req.Lines) > maxZohoLines {
		return "invalid_request", "lines exceed 100 line items"
	}
	for i, l := range req.Lines {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			return "invalid_request", "lines[" + itoa(i) + "].name is required"
		}
		if utf8.RuneCountInString(name) > maxZohoNameRunes {
			return "invalid_request", "lines[" + itoa(i) + "].name exceeds 128 characters"
		}
		if l.Quantity < 1 || l.Quantity > maxZohoQuantity {
			return "invalid_request", "lines[" + itoa(i) + "].quantity must be between 1 and 1000000"
		}
		if l.AmountPaise <= 0 {
			return "invalid_request", "lines[" + itoa(i) + "].amount_paise must be positive (an invoice line is a charge)"
		}
		if l.AmountPaise > maxZohoAmountPaise {
			return "invalid_request", "lines[" + itoa(i) + "].amount_paise exceeds the ₹10 crore per-line ceiling"
		}
	}
	return "", ""
}

// ZohoHandler serves the two Zoho routes for one API instance.
type ZohoHandler struct {
	pool *db.Pool
	// provider is the deployment-wide provider (the fail-closed stub
	// until the post-investors Zoho OAuth onboarding).
	provider ZohoBooksProvider
	log      *slog.Logger
	// sellerID resolves the OIDC subject to the seller row id the
	// caller owns (same seam as the WhatsApp/Tally handlers).
	sellerID func(ctx context.Context, subject string) (string, error)
}

// NewZohoHandler builds the handler. provider may be nil (tests, or a
// deployment that wired the routes before the provider seam): the
// routes then answer 503.
func NewZohoHandler(pool *db.Pool, provider ZohoBooksProvider, log *slog.Logger) *ZohoHandler {
	return &ZohoHandler{
		pool:     pool,
		provider: provider,
		log:      log,
		sellerID: resolveSeller(pool),
	}
}

// decodeZohoBody parses one request body with strict unknown-field
// rejection; it answers the envelope error itself and reports whether
// the caller should continue.
func (h *ZohoHandler) decodeZohoBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxZohoBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_request", "request body must be valid JSON with known fields only")
		return false
	}
	return true
}

// requireSeller answers 401/403 for a caller without a seller row; it
// returns the seller id (unused by the provider seam — the point is
// the scoping check) when the caller is a seller.
func (h *ZohoHandler) requireSeller(w http.ResponseWriter, r *http.Request) string {
	subject := middleware.SubjectFromContext(r.Context())
	if subject == "" {
		middleware.WriteStatusError(w, r, http.StatusUnauthorized, "unauthorized")
		return ""
	}
	sellerID, err := h.sellerID(r.Context(), subject)
	if err != nil {
		if errors.Is(err, errNoSellerRow) {
			middleware.WriteJSONError(w, r, http.StatusForbidden,
				"not_a_seller", "Zoho Books operations are seller-scoped: register a seller account first")
			return ""
		}
		h.log.Error("zoho: seller resolution failed", "err", err,
			"request_id", middleware.FromContext(r.Context()))
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return ""
	}
	return sellerID
}

// providerErr maps a provider-layer failure onto the honest HTTP
// answer: a stub seam failure (or a nil provider) is a fail-closed
// 503; anything else is a 502 from the upstream.
func (h *ZohoHandler) providerErr(w http.ResponseWriter, r *http.Request, err error, ref string) {
	var dis *ErrIntegrationDisabled
	if h.provider == nil || errors.As(err, &dis) {
		w.Header().Set("Retry-After", "60")
		middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
			"zoho_unavailable", "Zoho Books is disabled on this deployment — the OAuth onboarding is post-investors (see docs/integrations.md)")
		return
	}
	h.log.Error("zoho: provider call failed", "err", err,
		"reference_no", ref, "request_id", middleware.FromContext(r.Context()))
	middleware.WriteJSONError(w, r, http.StatusBadGateway,
		"zoho_upstream", "Zoho Books failed to accept the request")
}

// CreateContact serves POST /v1/integrations/zoho/contacts.
func (h *ZohoHandler) CreateContact() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.requireSeller(w, r) == "" {
			return
		}
		var req ZohoContactRequest
		if !h.decodeZohoBody(w, r, &req) {
			return
		}
		if code, msg := validateZohoContact(&req); code != "" {
			status := http.StatusBadRequest
			if code == "invalid_phone" {
				status = http.StatusUnprocessableEntity
			}
			middleware.WriteJSONError(w, r, status, code, msg)
			return
		}
		contact := ZohoContact{
			DisplayName: strings.TrimSpace(req.DisplayName),
			Email:       strings.TrimSpace(req.Email),
			Phone:       req.Phone,
		}
		contactID, err := h.callCreateContact(r.Context(), contact)
		if err != nil {
			h.providerErr(w, r, err, "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"contact_id": contactID})
	})
}

// callCreateContact keeps the nil-provider call out of the provider
// interface so the test suite can exercise the 503 path without a
// concrete provider.
func (h *ZohoHandler) callCreateContact(ctx context.Context, c ZohoContact) (string, error) {
	if h.provider == nil {
		return "", errZohoStub
	}
	return h.provider.CreateContact(ctx, c)
}

// CreateInvoice serves POST /v1/integrations/zoho/invoices.
func (h *ZohoHandler) CreateInvoice() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.requireSeller(w, r) == "" {
			return
		}
		var req ZohoInvoiceRequest
		if !h.decodeZohoBody(w, r, &req) {
			return
		}
		if code, msg := validateZohoInvoice(&req); code != "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, code, msg)
			return
		}
		due, _ := time.Parse("20060102", req.DueDate)
		invoice := ZohoInvoice{
			ContactID:   strings.TrimSpace(req.ContactID),
			ReferenceNo: strings.TrimSpace(req.ReferenceNo),
			Currency:    req.Currency,
			DueDate:     due,
		}
		for _, l := range req.Lines {
			invoice.Lines = append(invoice.Lines, ZohoInvoiceLine{
				Name:        strings.TrimSpace(l.Name),
				Quantity:    l.Quantity,
				AmountPaise: l.AmountPaise,
			})
		}
		invoiceID, err := h.callCreateInvoice(r.Context(), invoice)
		if err != nil {
			h.providerErr(w, r, err, invoice.ReferenceNo)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"invoice_id": invoiceID})
	})
}

// callCreateInvoice mirrors callCreateContact for the nil-provider
// 503 path.
func (h *ZohoHandler) callCreateInvoice(ctx context.Context, inv ZohoInvoice) (string, error) {
	if h.provider == nil {
		return "", errZohoStub
	}
	return h.provider.CreateInvoice(ctx, inv)
}
