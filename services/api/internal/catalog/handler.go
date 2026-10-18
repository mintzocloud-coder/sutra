// SPDX-License-Identifier: AGPL-3.0-or-later
package catalog

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxRegisterBody caps the register request: a store name, a slug and
// a GSTIN are small; anything bigger is abuse, not a registration.
const maxRegisterBody = 16 << 10

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// clientErr reports whether err is a mapped client error (4xx) rather
// than an unexpected (500) failure.
func clientErr(err error) bool {
	for _, e := range []error{
		ErrSellerExists, ErrSellerNotFound, ErrInvalidStoreName,
		ErrInvalidSlug, ErrSlugTaken, ErrInvalidGSTIN,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// catalogError maps a catalog error onto the standard envelope: 4xx
// for caller faults, 500 for everything else. Driver text never
// reaches the client — it goes to the log with the request id.
func catalogError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	var code string
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrSellerExists):
		status, code = http.StatusConflict, "seller_already_registered"
	case errors.Is(err, ErrSlugTaken):
		status, code = http.StatusConflict, "slug_taken"
	case errors.Is(err, ErrInvalidStoreName):
		status, code = http.StatusUnprocessableEntity, "invalid_store_name"
	case errors.Is(err, ErrInvalidSlug):
		status, code = http.StatusUnprocessableEntity, "invalid_slug"
	case errors.Is(err, ErrInvalidGSTIN):
		status, code = http.StatusUnprocessableEntity, "invalid_gstin"
	case errors.Is(err, ErrSellerNotFound):
		status, code = http.StatusNotFound, "seller_not_found"
	}
	if clientErr(err) {
		middleware.WriteJSONError(w, r, status, code, err.Error())
		return
	}
	log.Error("catalog: "+op+" failed", "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// registerRequest is the POST /v1/sellers body. store_slug is optional:
// empty means "derive it from the store name".
type registerRequest struct {
	StoreName string `json:"store_name"`
	StoreSlug string `json:"store_slug"`
	GSTIN     string `json:"gstin"`
}

// RegisterSellerHandler serves POST /v1/sellers: the caller registers
// their seller account (first-come; one per user). GSTIN is optional —
// Wave 1 accepts the shape; verification is post-incorporation.
func RegisterSellerHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}

		var raw map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBody))
		if err := dec.Decode(&raw); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_json", "request body must be a JSON object")
			return
		}
		// Strict API: unknown keys fail loudly instead of being
		// silently ignored.
		for key := range raw {
			switch key {
			case "store_name", "store_slug", "gstin":
			default:
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"unknown_field", "unknown field: "+key)
				return
			}
		}

		var req registerRequest
		for key, val := range raw {
			if string(val) == "null" {
				continue // explicit null = field not provided
			}
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field "+key+" must be a string or null")
				return
			}
			switch key {
			case "store_name":
				req.StoreName = s
			case "store_slug":
				req.StoreSlug = strings.ToLower(s)
			case "gstin":
				req.GSTIN = strings.ToUpper(strings.TrimSpace(s))
			}
		}

		seller, err := svc.Register(r.Context(), userID, req.StoreName, req.StoreSlug, req.GSTIN)
		if err != nil {
			catalogError(w, r, log, "register seller", err)
			return
		}
		writeJSON(w, http.StatusCreated, seller)
	}
}

// GetSellerHandler serves GET /v1/sellers/{id}: the public storefront
// record. 400 on non-UUID ids without touching the database.
func GetSellerHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_seller_id", "seller id must be a UUID")
			return
		}
		seller, err := svc.Get(r.Context(), id)
		if err != nil {
			catalogError(w, r, log, "get seller", err)
			return
		}
		writeJSON(w, http.StatusOK, seller)
	}
}

// ListCategoriesHandler serves GET /v1/categories: the curated
// category tree (roots with nested children). Public; no auth payload
// beyond the standard /v1/* token gate.
func ListCategoriesHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tree, err := svc.ListCategories(r.Context())
		if err != nil {
			catalogError(w, r, log, "list categories", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"categories": tree})
	}
}
