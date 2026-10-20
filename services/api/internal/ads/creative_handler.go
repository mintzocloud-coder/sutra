// SPDX-License-Identifier: AGPL-3.0-or-later
// HTTP handlers for the creative registry (H3 part 3a).
//
// All routes mount under /v1/ behind RequireAuth, and all are
// seller-scoped through the same resolveAdsSeller seam the campaign
// handlers use: the advertiser is the caller's seller row, ownership
// is checked through the creative's campaign, and cross-owner access
// is 403 (never leaks existence).
//
// Routes:
//
//	POST   /v1/ads/campaigns/{id}/creatives  create (draft) → 201
//	GET    /v1/ads/campaigns/{id}/creatives  list my campaign's creatives → 200
//	GET    /v1/ads/creatives/{id}            read (owner, 404 archived) → 200
//	PATCH  /v1/ads/creatives/{id}            owner update → 200
//	DELETE /v1/ads/creatives/{id}            archive (owner) → 200 (archived record)
//	POST   /v1/ads/creatives/{id}/activate   draft → active → 200
package ads

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// CreativeHandler serves the creative routes. The sellerID seam is the
// same one the campaign handler uses (stubbed in tests); svc is the
// registry service.
type CreativeHandler struct {
	pool     *db.Pool
	svc      *CreativeService
	log      *slog.Logger
	sellerID func(ctx context.Context, subject string) (pgtype.UUID, error)
}

// NewCreativeHandler builds the creative handler.
func NewCreativeHandler(pool *db.Pool, svc *CreativeService, log *slog.Logger) *CreativeHandler {
	return &CreativeHandler{
		pool:     pool,
		svc:      svc,
		log:      log,
		sellerID: resolveAdsSeller(pool),
	}
}

// creativeClientErr reports whether err is a mapped client error (4xx).
func creativeClientErr(err error) bool {
	for _, e := range []error{
		ErrCreativeNotFound, ErrCreativeNotOwner, ErrInvalidCreativeField,
		ErrBadCreativeTransition, ErrInvalidCreativeCursor, ErrCreativeKindLocked,
		ErrCampaignNotFound, ErrCampaignNotOwner, errAdsNoSeller,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// creativeError maps package errors to envelope responses.
func (h *CreativeHandler) creativeError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if creativeClientErr(err) {
		status := http.StatusBadRequest
		code := "bad_request"
		msg := "invalid request"
		switch {
		case errors.Is(err, ErrCreativeNotFound):
			status, code, msg = http.StatusNotFound, "creative_not_found", "creative not found"
		case errors.Is(err, ErrCreativeNotOwner):
			status, code, msg = http.StatusForbidden, "not_owner", "this creative belongs to another advertiser"
		case errors.Is(err, errAdsNoSeller):
			status, code, msg = http.StatusForbidden, "not_a_seller", "ads are seller-scoped: register a seller account first"
		case errors.Is(err, ErrCampaignNotFound):
			status, code, msg = http.StatusNotFound, "campaign_not_found", "campaign not found"
		case errors.Is(err, ErrCampaignNotOwner):
			status, code, msg = http.StatusForbidden, "not_owner", "this campaign belongs to another advertiser"
		case errors.Is(err, ErrBadCreativeTransition):
			status, code, msg = http.StatusUnprocessableEntity, "invalid_status_transition", "this status change is not allowed"
		case errors.Is(err, ErrCreativeKindLocked):
			status, code, msg = http.StatusUnprocessableEntity, "kind_locked", "kind and format are immutable once active"
		case errors.Is(err, ErrInvalidCreativeField):
			code, msg = "invalid_field", err.Error()
		case errors.Is(err, ErrInvalidCreativeCursor):
			code, msg = "invalid_pagination", "limit must be a positive integer and cursor must be valid"
		}
		middleware.WriteJSONError(w, r, status, code, msg)
		return
	}
	h.log.Error("ads: "+op, "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// creativeScope resolves the caller's seller row (the advertiser).
func (h *CreativeHandler) creativeScope(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	subject := middleware.SubjectFromContext(r.Context())
	sellerID, err := h.sellerID(r.Context(), subject)
	if err != nil {
		if errors.Is(err, errAdsNoSeller) {
			middleware.WriteJSONError(w, r, http.StatusForbidden,
				"not_a_seller", "ads are seller-scoped: register a seller account first")
			return pgtype.UUID{}, false
		}
		h.creativeError(w, r, "resolve seller", err)
		return pgtype.UUID{}, false
	}
	return sellerID, true
}

// campaignPathUUID parses the {id} path value of the nested
// /v1/ads/campaigns/{id}/creatives routes as a UUID.
func campaignPathUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_campaign_id", "campaign id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// creativePathUUID parses the {id} path value of the
// /v1/ads/creatives/{id} routes as a UUID.
func creativePathUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_creative_id", "creative id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// CampaignCreativesMux serves the nested collection routes:
// POST+GET /v1/ads/campaigns/{id}/creatives.
func (h *CreativeHandler) CampaignCreativesMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.create(w, r)
		case http.MethodGet:
			h.list(w, r)
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(w, r)
		}
	}
}

// CreativeMux serves the item routes: GET/PATCH/DELETE
// /v1/ads/creatives/{id}. Activation lives on the explicit action
// route (activate) registered in main.go, like campaigns.
func (h *CreativeHandler) CreativeMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := creativePathUUID(w, r)
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			advertiser, ok := h.creativeScope(w, r)
			if !ok {
				return
			}
			c, err := h.svc.Get(r.Context(), advertiser, id)
			if err != nil {
				h.creativeError(w, r, "get creative", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		case http.MethodPatch:
			advertiser, ok := h.creativeScope(w, r)
			if !ok {
				return
			}
			var in CreativePatch
			if !decodeAdsBody(w, r, &in) {
				return
			}
			c, err := h.svc.Update(r.Context(), advertiser, id, in)
			if err != nil {
				h.creativeError(w, r, "update creative", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		case http.MethodDelete:
			advertiser, ok := h.creativeScope(w, r)
			if !ok {
				return
			}
			// Archive, like the campaign surface: the 200 carries the
			// archived record so the client can reflect the terminal
			// state without a refetch.
			c, err := h.svc.Transition(r.Context(), advertiser, id, CreativeArchived)
			if err != nil {
				h.creativeError(w, r, "archive creative", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		default:
			middleware.MethodNotAllowedHandler(http.MethodGet, http.MethodPatch, http.MethodDelete)(w, r)
		}
	}
}

// ActivateTo serves POST /v1/ads/creatives/{id}/activate:
// draft → active. (Archive is DELETE on the item route.)
func (h *CreativeHandler) ActivateTo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := creativePathUUID(w, r)
		if !ok {
			return
		}
		advertiser, ok := h.creativeScope(w, r)
		if !ok {
			return
		}
		c, err := h.svc.Transition(r.Context(), advertiser, id, CreativeActive)
		if err != nil {
			h.creativeError(w, r, "activate creative", err)
			return
		}
		writeAdsJSON(w, http.StatusOK, c)
	}
}

// create serves POST /v1/ads/campaigns/{id}/creatives: register a
// draft creative under the caller's campaign.
func (h *CreativeHandler) create(w http.ResponseWriter, r *http.Request) {
	campaignID, ok := campaignPathUUID(w, r)
	if !ok {
		return
	}
	advertiser, ok := h.creativeScope(w, r)
	if !ok {
		return
	}
	var in CreativeInput
	if !decodeAdsBody(w, r, &in) {
		return
	}
	c, err := h.svc.Create(r.Context(), advertiser, campaignID, in)
	if err != nil {
		h.creativeError(w, r, "create creative", err)
		return
	}
	writeAdsJSON(w, http.StatusCreated, c)
}

// list serves GET /v1/ads/campaigns/{id}/creatives: the campaign's
// creatives, newest first, keyset-paginated.
func (h *CreativeHandler) list(w http.ResponseWriter, r *http.Request) {
	campaignID, ok := campaignPathUUID(w, r)
	if !ok {
		return
	}
	advertiser, ok := h.creativeScope(w, r)
	if !ok {
		return
	}
	limit := DefaultAdsPageLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_pagination", "limit must be a positive integer and cursor must be valid")
			return
		}
		if n > MaxAdsPageLimit {
			n = MaxAdsPageLimit
		}
		limit = n
	}
	var after *AdsCursor
	if s := r.URL.Query().Get("cursor"); s != "" {
		c, err := DecodeAdsCursor(s)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_pagination", "limit must be a positive integer and cursor must be valid")
			return
		}
		after = &c
	}
	page, err := h.svc.List(r.Context(), advertiser, campaignID, limit, after)
	if err != nil {
		h.creativeError(w, r, "list creatives", err)
		return
	}
	writeAdsJSON(w, http.StatusOK, page)
}
