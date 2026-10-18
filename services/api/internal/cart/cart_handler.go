// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 1): HTTP handlers for the cart.
//
// Routes (all behind RequireAuth in main.go):
//
//	GET    /v1/cart                 view cart (200, empty cart included)
//	DELETE /v1/cart                 clear cart (204, idempotent)
//	POST   /v1/cart/items           add line (201; quantity accumulates)
//	PATCH  /v1/cart/items/{variant} set line quantity (200; 0 removes → 204)
//	DELETE /v1/cart/items/{variant} remove line (204, idempotent)
//
// Bodies follow the strict contract used across the commerce API:
// unknown fields fail loudly, explicit null is "not provided".
package cart

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxCartBody caps cart request bodies: variant ids and quantities are
// a few hundred bytes at most.
const maxCartBody = 4 << 10

// decodeStrict decodes a JSON object body with a size cap and a field
// allowlist, mirroring the catalog commerce handlers.
func decodeStrict(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCartBody))
	if err := dec.Decode(&raw); err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_json", "request body must be a JSON object")
		return nil, false
	}
	for key := range raw {
		if !allowed[key] {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"unknown_field", "unknown field: "+key)
			return nil, false
		}
	}
	return raw, true
}

// cartError maps a cart error onto the standard JSON error envelope.
func cartError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	status, code := cartErrorCode(err)
	if status == http.StatusInternalServerError {
		log.Error("cart: "+op+" failed", "err", err,
			"request_id", middleware.FromContext(r.Context()))
		middleware.WriteStatusError(w, r, status, "internal server error")
		return
	}
	middleware.WriteJSONError(w, r, status, code, err.Error())
}

func cartErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidQuantity):
		return http.StatusUnprocessableEntity, "invalid_quantity"
	case errors.Is(err, ErrVariantNotFound):
		return http.StatusNotFound, "variant_not_found"
	case errors.Is(err, ErrVariantUnavailable):
		return http.StatusUnprocessableEntity, "variant_not_available"
	case errors.Is(err, ErrInsufficientStock):
		return http.StatusConflict, "insufficient_stock"
	case errors.Is(err, ErrItemNotFound):
		return http.StatusNotFound, "cart_item_not_found"
	}
	return http.StatusInternalServerError, ""
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// variantPathID parses the {variant_id} path value into a UUID,
// 400 on garbage without touching the database.
func variantPathID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := social.ParseUserID(r.PathValue("variant_id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_variant_id", "variant id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// CartHandler serves GET /v1/cart (view) and DELETE /v1/cart (clear);
// wrong methods get the standard 405 envelope with an Allow header.
func CartHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodDelete:
		default:
			middleware.MethodNotAllowedHandler(http.MethodGet, http.MethodDelete)(w, r)
			return
		}
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		if r.Method == http.MethodDelete {
			if err := svc.Clear(r.Context(), userID); err != nil {
				cartError(w, r, log, "clear cart", err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		c, err := svc.Get(r.Context(), userID)
		if err != nil {
			cartError(w, r, log, "view cart", err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

// AddItemHandler serves POST /v1/cart/items: add qty units of a variant
// to the caller's cart, accumulating with any existing line.
func AddItemHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"variant_id": true, "quantity": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		raw, ok := decodeStrict(w, r, allowed)
		if !ok {
			return
		}
		var variantStr string
		if v, present := raw["variant_id"]; !present || string(v) == "null" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "variant_id is required")
			return
		} else if err := json.Unmarshal(v, &variantStr); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant_id must be a string UUID")
			return
		}
		variantID, err := social.ParseUserID(variantStr)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant_id must be a UUID")
			return
		}
		var qty int
		if v, present := raw["quantity"]; !present || string(v) == "null" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "quantity is required")
			return
		} else if err := json.Unmarshal(v, &qty); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_quantity", "quantity must be an integer")
			return
		}
		ln, err := svc.AddItem(r.Context(), userID, variantID, qty)
		if err != nil {
			cartError(w, r, log, "add cart item", err)
			return
		}
		writeJSON(w, http.StatusCreated, ln)
	}
}

// CartItemHandler serves PATCH /v1/cart/items/{variant_id} (set the line
// quantity; 0 removes the line → 204) and DELETE
// /v1/cart/items/{variant_id} (remove the line → 204, idempotent).
func CartItemHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch, http.MethodDelete:
		default:
			middleware.MethodNotAllowedHandler(http.MethodPatch, http.MethodDelete)(w, r)
			return
		}
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		variantID, ok := variantPathID(w, r)
		if !ok {
			return
		}
		if r.Method == http.MethodDelete {
			if err := svc.RemoveItem(r.Context(), userID, variantID); err != nil {
				cartError(w, r, log, "remove cart item", err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// PATCH: body {quantity}.
		raw, ok := decodeStrict(w, r, map[string]bool{"quantity": true})
		if !ok {
			return
		}
		var qty int
		if v, present := raw["quantity"]; !present || string(v) == "null" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "quantity is required")
			return
		} else if err := json.Unmarshal(v, &qty); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_quantity", "quantity must be an integer")
			return
		}
		if qty == 0 {
			if err := svc.RemoveItem(r.Context(), userID, variantID); err != nil {
				cartError(w, r, log, "remove cart item", err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		ln, err := svc.SetQuantity(r.Context(), userID, variantID, qty)
		if err != nil {
			cartError(w, r, log, "set cart item quantity", err)
			return
		}
		writeJSON(w, http.StatusOK, ln)
	}
}
