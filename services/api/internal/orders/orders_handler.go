// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 2a): HTTP handlers for orders.
//
// Routes (all behind RequireAuth in main.go):
//
//	POST   /v1/orders                 checkout cart → orders (201, COD)
//	GET    /v1/orders                 list (200; ?role=buyer|seller)
//	GET    /v1/orders/{id}            order detail (200)
//	PATCH  /v1/orders/{id}/status     transition (200; {"status": ...})
//
// Bodies follow the strict contract used across the commerce API:
// unknown fields fail loudly, explicit null is "not provided".
package orders

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxOrdersBody caps order request bodies: status names are short.
const maxOrdersBody = 4 << 10

// decodeStrict decodes a JSON object body with a size cap and a field
// allowlist, mirroring the cart handlers. An absent body decodes as
// an empty object (checkout takes no parameters).
func decodeStrict(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (map[string]json.RawMessage, bool) {
	raw := map[string]json.RawMessage{}
	if r.ContentLength == 0 {
		return raw, true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOrdersBody))
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

// orderError maps an orders error onto the standard JSON error envelope.
func orderError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	status, code := orderErrorCode(err)
	if status == http.StatusInternalServerError {
		log.Error("orders: "+op+" failed", "err", err,
			"request_id", middleware.FromContext(r.Context()))
		middleware.WriteStatusError(w, r, status, "internal server error")
		return
	}
	middleware.WriteJSONError(w, r, status, code, err.Error())
}

func orderErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, ErrCartEmpty):
		return http.StatusUnprocessableEntity, "cart_empty"
	case errors.Is(err, ErrOrderNotFound):
		return http.StatusNotFound, "order_not_found"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrInvalidStatus):
		return http.StatusUnprocessableEntity, "invalid_status"
	case errors.Is(err, ErrInvalidTransition):
		return http.StatusUnprocessableEntity, "invalid_transition"
	case errors.Is(err, ErrVariantUnavailable):
		return http.StatusUnprocessableEntity, "variant_not_available"
	case errors.Is(err, ErrInsufficientStock):
		return http.StatusConflict, "insufficient_stock"
	}
	return http.StatusInternalServerError, ""
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// orderPathID parses the {id} path value into a UUID,
// 400 on garbage without touching the database.
func orderPathID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := social.ParseUserID(r.PathValue("id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_order_id", "order id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// pageParams parses ?limit= and ?offset= with sane defaults and caps;
// garbage is a 400, not a silent clamp.
func pageParams(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	q := r.URL.Query()
	limit, offset = 20, 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_limit", "limit must be an integer")
			return 0, 0, false
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_offset", "offset must be an integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// OrdersHandler serves POST /v1/orders (checkout) and GET /v1/orders
// (list); wrong methods get the standard 405 envelope with an Allow
// header.
func OrdersHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodGet:
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(w, r)
			return
		}
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		if r.Method == http.MethodGet {
			limit, offset, ok := pageParams(w, r)
			if !ok {
				return
			}
			var (
				sums []*Summary
				err  error
			)
			switch r.URL.Query().Get("role") {
			case "", "buyer":
				sums, err = svc.ListBuyer(r.Context(), userID, limit, offset)
			case "seller":
				sums, err = svc.ListSeller(r.Context(), userID, limit, offset)
			default:
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_role", "role must be buyer or seller")
				return
			}
			if err != nil {
				orderError(w, r, log, "list orders", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"orders": sums})
			return
		}
		// Checkout takes no parameters; an absent body is normal.
		if _, ok := decodeStrict(w, r, map[string]bool{}); !ok {
			return
		}
		orders, err := svc.Checkout(r.Context(), userID)
		if err != nil {
			orderError(w, r, log, "checkout", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"orders": orders})
	}
}

// OrderDetailHandler serves GET /v1/orders/{id} (full order view for
// the buyer or the seller only).
func OrderDetailHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		orderID, ok := orderPathID(w, r)
		if !ok {
			return
		}
		ord, err := svc.Get(r.Context(), userID, orderID)
		if err != nil {
			orderError(w, r, log, "get order", err)
			return
		}
		writeJSON(w, http.StatusOK, ord)
	}
}

// OrderStatusHandler serves PATCH /v1/orders/{id}/status:
// {"status": "confirmed"} moves the order along the state machine.
// The caller must be the buyer or the seller, and the (from, to,
// role) edge must exist.
func OrderStatusHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"status": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		orderID, ok := orderPathID(w, r)
		if !ok {
			return
		}
		raw, ok := decodeStrict(w, r, allowed)
		if !ok {
			return
		}
		var status string
		if v, present := raw["status"]; !present || string(v) == "null" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "status is required")
			return
		} else if err := json.Unmarshal(v, &status); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_status", "status must be a string")
			return
		}
		ord, err := svc.Transition(r.Context(), userID, orderID, status)
		if err != nil {
			orderError(w, r, log, "transition order", err)
			return
		}
		writeJSON(w, http.StatusOK, ord)
	}
}
