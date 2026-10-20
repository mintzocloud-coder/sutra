// SPDX-License-Identifier: AGPL-3.0-or-later
// Publisher serving endpoint (H3 part 3b chunk 2): the HTTP face of the
// ad-serving decision path.
//
//	POST /v1/ads/serve  publisher slot → winning creative (or no ad)
//
// The request carries SLOT CONTENT only (slot name, page categories,
// content language, publisher country) — never identity: the contextual
// > identity rule from docs/ads.md is enforced by the request shape.
// The endpoint runs behind the same RequireAuth as every /v1/ route;
// the publisher surface calls it with its own credential (a dedicated
// publisher API key is a later serving pass — see docs/ads.md).
//
// Responses:
//
//	200  a creative won — the body is the ServedAd (impression_id,
//	     campaign/creative ids, GSP price_paise, 55/45 split, the
//	     creative to render). The spend was already accrued in the
//	     same transaction (billImpression) — the 200 means billed.
//	204  nothing servable (no eligible campaign, no active creative,
//	     all bids below reserve, or a budget race at billing time) —
//	     the publisher renders organic content. A normal outcome, not
//	     an error.
//	503  serving_disabled — ADS_SERVING_ENABLED is off. LOUD, not
//	     silent: a half-wired integration must never look like "no
//	     demand".
//	400  invalid_json (unknown fields rejected) or invalid_serve_request.
package ads

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// serveRequestLimits bound the publisher-supplied slot context.
const (
	maxServeSlotLen     = 64
	maxServeCategories  = 10
	maxServeCategoryLen = 64
	maxServeLanguageLen = 5
	maxServeCountryLen  = 2
)

// ServeRequest is the POST /v1/ads/serve body. Content, never
// identity — the struct has nowhere to put a user ID, a device
// fingerprint, or a behavioral signal.
type ServeRequest struct {
	Slot       string   `json:"slot"`
	Categories []string `json:"categories"`
	Language   string   `json:"language"`
	Country    string   `json:"country"`
}

// toSlot validates the request into the serving seam's SlotContext.
// Unknown or out-of-shape input fails LOUD: silently matching every
// campaign would spend real advertiser money.
func (r ServeRequest) toSlot() (SlotContext, error) {
	slot := strings.TrimSpace(r.Slot)
	if slot == "" || len(slot) > maxServeSlotLen {
		return SlotContext{}, errInvalidServeRequest
	}
	if len(r.Categories) > maxServeCategories {
		return SlotContext{}, errInvalidServeRequest
	}
	cats := make([]string, 0, len(r.Categories))
	for _, c := range r.Categories {
		c = strings.TrimSpace(c)
		if c == "" || len(c) > maxServeCategoryLen {
			return SlotContext{}, errInvalidServeRequest
		}
		cats = append(cats, c)
	}
	if len(r.Language) > maxServeLanguageLen || len(r.Country) > maxServeCountryLen {
		return SlotContext{}, errInvalidServeRequest
	}
	return SlotContext{
		Slot:       slot,
		Categories: cats,
		Language:   strings.TrimSpace(r.Language),
		Country:    strings.ToUpper(strings.TrimSpace(r.Country)),
	}, nil
}

// errInvalidServeRequest is the package error for a well-formed-JSON
// but out-of-shape serve request.
var errInvalidServeRequest = errNew("invalid serve request: slot is required, categories/languages/countries are bounded")

// errNew is a tiny package-error helper (kept here so the handler
// stays independent of the campaign error tables).
type pkgError struct{ msg string }

func (e pkgError) Error() string { return e.msg }

func errNew(msg string) error { return pkgError{msg: msg} }

// ServeHandler serves POST /v1/ads/serve.
type ServeHandler struct {
	pool *db.Pool
	deps ServingDeps
	cfg  ServingConfig
	log  *slog.Logger
}

// NewServeHandler builds the serve handler. cfg is read once at boot
// (ServingConfigFromEnv in main.go); toggling the env needs a restart,
// like every other fail-closed seam.
func NewServeHandler(pool *db.Pool, deps ServingDeps, cfg ServingConfig, log *slog.Logger) *ServeHandler {
	return &ServeHandler{pool: pool, deps: deps, cfg: cfg, log: log}
}

// ServeHTTP serves POST /v1/ads/serve (method discipline is enforced by
// methodGuard at registration; this handler defends in depth).
func (h *ServeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		middleware.MethodNotAllowedHandler(http.MethodPost)(w, r)
		return
	}
	var in ServeRequest
	if !decodeAdsBody(w, r, &in) {
		return
	}
	slot, err := in.toSlot()
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_serve_request", "slot is required; categories, language and country are bounded")
		return
	}
	served, err := Serve(r.Context(), h.pool, h.log, h.deps, h.cfg, time.Now().UTC(), slot)
	if err != nil {
		switch {
		case err == ErrServingDisabled:
			// LOUD fail-closed: the publisher must know the seam is
			// off instead of reading "no demand" silence.
			middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
				"serving_disabled", "ads serving is disabled (ADS_SERVING_ENABLED)")
			return
		case err == ErrBudgetRaced:
			// The billing transaction rolled back cleanly; the
			// documented contract is "no ad served".
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if h.log != nil {
			h.log.Error("ads: serve", "err", err,
				"request_id", middleware.FromContext(r.Context()))
		}
		middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if served == nil {
		w.WriteHeader(http.StatusNoContent) // no ad — render organic
		return
	}
	writeAdsJSON(w, http.StatusOK, served)
}
