// SPDX-License-Identifier: AGPL-3.0-or-later
// HTTP handlers for the advertiser API (H3 part 2).
//
// All routes mount under /v1/ behind RequireAuth, so the OIDC subject
// is always present. Routes are seller-scoped: the advertiser is the
// caller's seller row (register one via POST /v1/sellers first), so a
// non-seller gets 403 not_a_seller and one seller can never see
// another's campaigns.
//
// Routes:
//
//	POST   /v1/ads/campaigns                 create (draft) → 201
//	GET    /v1/ads/campaigns                 my campaigns → 200
//	GET    /v1/ads/campaigns/{id}            read (owner, 404 archived) → 200
//	PATCH  /v1/ads/campaigns/{id}            owner update → 200
//	DELETE /v1/ads/campaigns/{id}            archive (owner) → 200 (archived record)
//	POST   /v1/ads/campaigns/{id}/activate   draft/paused → active → 200
//	POST   /v1/ads/campaigns/{id}/pause      active → paused → 200
//	POST   /v1/ads/campaigns/{id}/resume     paused → active → 200
package ads

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// maxAdsBody caps advertiser JSON bodies: metadata only, media never
// touches this surface.
const maxAdsBody = 64 << 10

const (
	// DefaultAdsPageLimit is the campaign list page size when the client
	// omits it; MaxAdsPageLimit clamps explicit requests (cost guard).
	DefaultAdsPageLimit = 20
	MaxAdsPageLimit     = 100
)

// errAdsNoSeller is the sentinel the sellerID seam returns when the
// caller has no seller row.
var errAdsNoSeller = errors.New("ads: caller has no seller account")

// CampaignHandler serves the advertiser routes. sellerID is a
// plain func so tests can stub it instead of touching the database;
// resolveAdsSeller is the production seam.
type CampaignHandler struct {
	pool     *db.Pool
	svc      *CampaignService
	log      *slog.Logger
	sellerID func(ctx context.Context, subject string) (pgtype.UUID, error)
}

// NewCampaignHandler builds the advertiser handler.
func NewCampaignHandler(pool *db.Pool, svc *CampaignService, log *slog.Logger) *CampaignHandler {
	return &CampaignHandler{
		pool:     pool,
		svc:      svc,
		log:      log,
		sellerID: resolveAdsSeller(pool),
	}
}

// resolveAdsSeller is the production sellerID seam: provision the user,
// then require a seller row. Mirrors the integrations pattern.
func resolveAdsSeller(pool *db.Pool) func(ctx context.Context, subject string) (pgtype.UUID, error) {
	return func(ctx context.Context, subject string) (pgtype.UUID, error) {
		u, err := users.Provision(ctx, pool, subject, "")
		if err != nil {
			return pgtype.UUID{}, err
		}
		var id pgtype.UUID
		if err := id.Scan(u.ID); err != nil {
			return pgtype.UUID{}, err
		}
		var sellerID pgtype.UUID
		if err := pool.QueryRow(ctx,
			`SELECT id FROM sellers WHERE user_id = $1`, id).
			Scan(&sellerID); err != nil {
			return pgtype.UUID{}, errAdsNoSeller
		}
		return sellerID, nil
	}
}

// targetUUID parses the {id} path value as a UUID.
func targetCampaignUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(r.PathValue("id")); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_campaign_id", "campaign id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// clientErr reports whether err is a mapped client error (4xx) rather
// than an unexpected (500) failure.
func clientErr(err error) bool {
	for _, e := range []error{
		ErrCampaignNotFound, ErrCampaignNotOwner, ErrInvalidCampaignField,
		ErrInvalidTargeting, ErrBadCampaignTransition, ErrUnknownCategorySlug,
		ErrInvalidCampaignCursor, errAdsNoSeller,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// adsError maps package errors to envelope responses; unexpected
// errors are logged and answered 500.
func (h *CampaignHandler) adsError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if clientErr(err) {
		status := http.StatusBadRequest
		code := "bad_request"
		msg := "invalid request"
		switch {
		case errors.Is(err, ErrCampaignNotFound):
			status, code, msg = http.StatusNotFound, "campaign_not_found", "campaign not found"
		case errors.Is(err, ErrCampaignNotOwner):
			status, code, msg = http.StatusForbidden, "not_owner", "this campaign belongs to another advertiser"
		case errors.Is(err, errAdsNoSeller):
			status, code, msg = http.StatusForbidden, "not_a_seller", "ads are seller-scoped: register a seller account first"
		case errors.Is(err, ErrInvalidCampaignField):
			code, msg = "invalid_field", err.Error()
		case errors.Is(err, ErrInvalidTargeting):
			code, msg = "invalid_targeting", err.Error()
		case errors.Is(err, ErrUnknownCategorySlug):
			code, msg = "unknown_category", err.Error()
		case errors.Is(err, ErrBadCampaignTransition):
			status, code, msg = http.StatusUnprocessableEntity, "invalid_status_transition", "this status change is not allowed"
		case errors.Is(err, ErrInvalidCampaignCursor):
			code, msg = "invalid_pagination", "limit must be a positive integer and cursor must be valid"
		}
		middleware.WriteJSONError(w, r, status, code, msg)
		return
	}
	h.log.Error("ads: "+op, "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// decodeAdsBody reads a bounded JSON body with unknown fields rejected
// (the advertiser surface is strict: typos fail loudly).
func decodeAdsBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdsBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_json", "request body must be valid JSON with known fields only")
		return false
	}
	return true
}

// advertiseScope resolves the caller's seller row (the advertiser).
func (h *CampaignHandler) advertiseScope(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	subject := middleware.SubjectFromContext(r.Context())
	sellerID, err := h.sellerID(r.Context(), subject)
	if err != nil {
		if errors.Is(err, errAdsNoSeller) {
			middleware.WriteJSONError(w, r, http.StatusForbidden,
				"not_a_seller", "ads are seller-scoped: register a seller account first")
			return pgtype.UUID{}, false
		}
		h.adsError(w, r, "resolve seller", err)
		return pgtype.UUID{}, false
	}
	return sellerID, true
}

func writeAdsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// CampaignsMux serves POST /v1/ads/campaigns (create) and
// GET /v1/ads/campaigns (list my campaigns) on the collection route.
func (h *CampaignHandler) CampaignsMux() http.HandlerFunc {
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

// CampaignMux serves the item routes: GET/PATCH/DELETE
// /v1/ads/campaigns/{id}. Status transitions live on explicit action
// routes (activate/pause/resume) registered in main.go, so the OpenAPI
// coverage gate sees real paths — no {action} dispatch magic.
func (h *CampaignHandler) CampaignMux() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetCampaignUUID(w, r)
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			advertiser, ok := h.advertiseScope(w, r)
			if !ok {
				return
			}
			c, err := h.svc.Get(r.Context(), advertiser, id)
			if err != nil {
				h.adsError(w, r, "get campaign", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		case http.MethodPatch:
			advertiser, ok := h.advertiseScope(w, r)
			if !ok {
				return
			}
			var in UpdateInput
			if !decodeAdsBody(w, r, &in) {
				return
			}
			c, err := h.svc.Update(r.Context(), advertiser, id, in)
			if err != nil {
				h.adsError(w, r, "update campaign", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		case http.MethodDelete:
			advertiser, ok := h.advertiseScope(w, r)
			if !ok {
				return
			}
			// Archive, like the product surface: the 200 carries the
			// archived record so the client can reflect the terminal
			// state without a refetch.
			c, err := h.svc.Transition(r.Context(), advertiser, id, CampaignArchived)
			if err != nil {
				h.adsError(w, r, "archive campaign", err)
				return
			}
			writeAdsJSON(w, http.StatusOK, c)
		default:
			middleware.MethodNotAllowedHandler(http.MethodGet, http.MethodPatch, http.MethodDelete)(w, r)
		}
	}
}

// TransitionTo serves the explicit status-action routes:
// POST /v1/ads/campaigns/{id}/activate|pause|resume. Resume is pause's
// inverse: paused -> active through the same transition matrix as
// activate (Transition resolves the edge, never the route name).
func (h *CampaignHandler) TransitionTo(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetCampaignUUID(w, r)
		if !ok {
			return
		}
		advertiser, ok := h.advertiseScope(w, r)
		if !ok {
			return
		}
		c, err := h.svc.Transition(r.Context(), advertiser, id, to)
		if err != nil {
			h.adsError(w, r, "transition campaign", err)
			return
		}
		writeAdsJSON(w, http.StatusOK, c)
	}
}

// create serves POST /v1/ads/campaigns: register a draft campaign.
func (h *CampaignHandler) create(w http.ResponseWriter, r *http.Request) {
	advertiser, ok := h.advertiseScope(w, r)
	if !ok {
		return
	}
	var in CreateInput
	if !decodeAdsBody(w, r, &in) {
		return
	}
	c, err := h.svc.Create(r.Context(), advertiser, in)
	if err != nil {
		h.adsError(w, r, "create campaign", err)
		return
	}
	writeAdsJSON(w, http.StatusCreated, c)
}

// list serves GET /v1/ads/campaigns: the caller's campaigns, newest
// first, keyset-paginated. A malformed cursor or limit is 400 (fail
// loudly); an over-large limit clamps (cost guard).
func (h *CampaignHandler) list(w http.ResponseWriter, r *http.Request) {
	advertiser, ok := h.advertiseScope(w, r)
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
	page, err := h.svc.List(r.Context(), advertiser, limit, after)
	if err != nil {
		h.adsError(w, r, "list campaigns", err)
		return
	}
	writeAdsJSON(w, http.StatusOK, page)
}
