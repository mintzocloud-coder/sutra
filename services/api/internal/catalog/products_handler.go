// SPDX-License-Identifier: AGPL-3.0-or-later
// F1b (part 1): HTTP handlers for products, variants and inventory.
//
// The write routes (create/update/archive/adjust) are owner-only: the
// handler resolves the caller through social.CallerUUID and the
// service refuses anything outside their seller account. The read
// routes (get/list/inventory) are public storefront reads. Bodies use
// the same strict contract as seller registration: unknown fields
// fail loudly, explicit null means "not provided" (except mrp_paise,
// where explicit null clears the MRP).
package catalog

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxProductBody caps product/variant request bodies: a title, a
// description and a small attribute bag are kilobytes, not megabytes.
const maxProductBody = 64 << 10

// decodeStrict decodes a JSON object body with a size cap and a field
// allowlist. Unknown fields fail loudly (strict API contract); explicit
// nulls are left in the map so handlers can distinguish "not provided"
// from "cleared".
func decodeStrict(w http.ResponseWriter, r *http.Request, maxBytes int64, allowed map[string]bool) (map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
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

// optString extracts an optional string field. ok=false means the field
// was absent or explicit null.
func optString(raw map[string]json.RawMessage, key string) (val string, ok bool, err error) {
	v, present := raw[key]
	if !present || string(v) == "null" {
		return "", false, nil
	}
	if err := json.Unmarshal(v, &val); err != nil {
		return "", false, err
	}
	return val, true, nil
}

// optInt64 extracts an optional integer field.
func optInt64(raw map[string]json.RawMessage, key string) (val int64, ok bool, err error) {
	v, present := raw[key]
	if !present || string(v) == "null" {
		return 0, false, nil
	}
	if err := json.Unmarshal(v, &val); err != nil {
		return 0, false, err
	}
	return val, true, nil
}

// CreateProductHandler serves POST /v1/products: the caller lists a
// new product under their seller account. New listings start as
// "draft" — invisible on the storefront until activated.
func CreateProductHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"title": true, "description": true, "category_slug": true, "status": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		raw, ok := decodeStrict(w, r, maxProductBody, allowed)
		if !ok {
			return
		}
		var title, desc, catSlug, status string
		var present bool
		if title, present, _ = optString(raw, "title"); !present {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "title is required")
			return
		}
		desc, _, _ = optString(raw, "description")
		catSlug, _, _ = optString(raw, "category_slug")
		status, _, _ = optString(raw, "status")

		p, err := svc.CreateProduct(r.Context(), userID, title, desc, catSlug, status)
		if err != nil {
			catalogError(w, r, log, "create product", err)
			return
		}
		hooks.productWritten(r.Context(), p.ID)
		writeJSON(w, http.StatusCreated, p)
	}
}

// GetProductHandler serves GET /v1/products/{id}: the public product
// page. 400 on non-UUID ids without touching the database.
func GetProductHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_product_id", "product id must be a UUID")
			return
		}
		p, err := svc.GetProduct(r.Context(), id)
		if err != nil {
			catalogError(w, r, log, "get product", err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// ListProductsHandler serves GET /v1/products?seller_id=…: one seller's
// public listings, newest first, keyset-paged. seller_id is required
// (the storefront query); limit clamps 1..50; cursor is the last id of
// the previous page.
func ListProductsHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sellerID, err := social.ParseUserID(r.URL.Query().Get("seller_id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_seller_id", "seller_id must be a UUID")
			return
		}
		limit := DefaultListLimit
		if s := r.URL.Query().Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > MaxListLimit {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_limit", "limit must be 1-50")
				return
			}
			limit = n
		}
		cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
		if cursor != "" {
			if _, err := social.ParseUserID(cursor); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_cursor", "cursor must be a product UUID")
				return
			}
		}
		products, err := svc.ListProducts(r.Context(), sellerID, limit, cursor)
		if err != nil {
			catalogError(w, r, log, "list products", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"products": products})
	}
}

// UpdateProductHandler serves PATCH /v1/products/{id}: owner-only
// partial edit. category_slug "" clears the category.
func UpdateProductHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"title": true, "description": true, "category_slug": true, "status": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_product_id", "product id must be a UUID")
			return
		}
		raw, ok := decodeStrict(w, r, maxProductBody, allowed)
		if !ok {
			return
		}
		var patch ProductPatch
		if v, present, err := optString(raw, "title"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "title must be a string")
			return
		} else if present {
			patch.Title = &v
		}
		if v, present, err := optString(raw, "description"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "description must be a string")
			return
		} else if present {
			patch.Description = &v
		}
		if v, present, err := optString(raw, "category_slug"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "category_slug must be a string")
			return
		} else if present {
			patch.CategorySlug = &v
		}
		if v, present, err := optString(raw, "status"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "status must be a string")
			return
		} else if present {
			patch.Status = &v
		}

		p, err := svc.UpdateProduct(r.Context(), userID, id, patch)
		if err != nil {
			catalogError(w, r, log, "update product", err)
			return
		}
		hooks.productWritten(r.Context(), p.ID)
		writeJSON(w, http.StatusOK, p)
	}
}

// DeleteProductHandler serves DELETE /v1/products/{id}: owner-only
// archive (products are never hard-deleted — order history in later
// waves references them).
func DeleteProductHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_product_id", "product id must be a UUID")
			return
		}
		p, err := svc.ArchiveProduct(r.Context(), userID, id)
		if err != nil {
			catalogError(w, r, log, "archive product", err)
			return
		}
		hooks.productWritten(r.Context(), p.ID)
		writeJSON(w, http.StatusOK, p)
	}
}

// AddVariantHandler serves POST /v1/products/{id}/variants: the owner
// adds one purchasable SKU (price in integer paise). The inventory row
// (quantity 0) is created with it.
func AddVariantHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"sku": true, "price_paise": true, "mrp_paise": true, "attributes": true, "status": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_product_id", "product id must be a UUID")
			return
		}
		raw, ok := decodeStrict(w, r, maxProductBody, allowed)
		if !ok {
			return
		}
		sku, present, err := optString(raw, "sku")
		if err != nil || !present {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "sku is required and must be a string")
			return
		}
		price, present, err := optInt64(raw, "price_paise")
		if err != nil || !present {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "price_paise is required and must be an integer")
			return
		}
		var mrp *int64
		if v, present := raw["mrp_paise"]; present {
			if string(v) == "null" {
				mrp = nil // explicit null = no MRP
			} else {
				var n int64
				if err := json.Unmarshal(v, &n); err != nil {
					middleware.WriteJSONError(w, r, http.StatusBadRequest,
						"invalid_json", "mrp_paise must be an integer or null")
					return
				}
				mrp = &n
			}
		}
		attrs := map[string]string{}
		if v, present := raw["attributes"]; present && string(v) != "null" {
			if err := json.Unmarshal(v, &attrs); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "attributes must be a string→string object")
				return
			}
		}
		status, _, _ := optString(raw, "status")

		v, err := svc.AddVariant(r.Context(), userID, id, sku, price, mrp, attrs, status)
		if err != nil {
			catalogError(w, r, log, "add variant", err)
			return
		}
		// The new variant can change the listing's price — rebuild the
		// parent product's search document (best-effort, detached).
		hooks.productWritten(r.Context(), id.String())
		writeJSON(w, http.StatusCreated, v)
	}
}

// UpdateVariantHandler serves PATCH /v1/variants/{id}: owner-only
// partial edit. mrp_paise explicit null clears the MRP.
func UpdateVariantHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"sku": true, "price_paise": true, "mrp_paise": true, "attributes": true, "status": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant id must be a UUID")
			return
		}
		raw, ok := decodeStrict(w, r, maxProductBody, allowed)
		if !ok {
			return
		}
		var patch VariantPatch
		if v, present, err := optString(raw, "sku"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "sku must be a string")
			return
		} else if present {
			patch.SKU = &v
		}
		if n, present, err := optInt64(raw, "price_paise"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "price_paise must be an integer")
			return
		} else if present {
			patch.PricePaise = &n
		}
		if v, present := raw["mrp_paise"]; present {
			patch.SetMRP = true // explicit null clears; a number sets
			if string(v) != "null" {
				var n int64
				if err := json.Unmarshal(v, &n); err != nil {
					middleware.WriteJSONError(w, r, http.StatusBadRequest,
						"invalid_json", "mrp_paise must be an integer or null")
					return
				}
				patch.MrpPaise = &n
			}
		}
		if v, present := raw["attributes"]; present && string(v) != "null" {
			var attrs map[string]string
			if err := json.Unmarshal(v, &attrs); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "attributes must be a string→string object")
				return
			}
			patch.Attributes = &attrs
		}
		if v, present, err := optString(raw, "status"); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest, "invalid_json", "status must be a string")
			return
		} else if present {
			patch.Status = &v
		}

		v, err := svc.UpdateVariant(r.Context(), userID, id, patch)
		if err != nil {
			catalogError(w, r, log, "update variant", err)
			return
		}
		// Price/status edits change the search document — rebuild the
		// parent product's doc (best-effort, detached).
		hooks.productWritten(r.Context(), v.ProductID)
		writeJSON(w, http.StatusOK, v)
	}
}

// DeleteVariantHandler serves DELETE /v1/variants/{id}: owner-only
// archive (variants are never hard-deleted — orders reference them).
func DeleteVariantHandler(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant id must be a UUID")
			return
		}
		v, err := svc.ArchiveVariant(r.Context(), userID, id)
		if err != nil {
			catalogError(w, r, log, "archive variant", err)
			return
		}
		// Archiving can remove the cheapest variant — rebuild the
		// parent product's search document (best-effort, detached).
		hooks.productWritten(r.Context(), v.ProductID)
		writeJSON(w, http.StatusOK, v)
	}
}

// GetInventoryHandler serves GET /v1/variants/{id}/inventory: the
// public stock record (buyers see availability).
func GetInventoryHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant id must be a UUID")
			return
		}
		inv, err := svc.GetInventory(r.Context(), id)
		if err != nil {
			catalogError(w, r, log, "get inventory", err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

// AdjustInventoryHandler serves POST /v1/variants/{id}/inventory/adjust:
// owner-only signed stock movement (delta may be negative; the write
// is atomic and refuses to drive quantity below zero).
func AdjustInventoryHandler(pool *db.Pool, svc *Service, log *slog.Logger) http.HandlerFunc {
	allowed := map[string]bool{"delta": true}
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := social.CallerUUID(w, r, pool, log)
		if !ok {
			return
		}
		id, err := social.ParseUserID(r.PathValue("id"))
		if err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_variant_id", "variant id must be a UUID")
			return
		}
		raw, ok := decodeStrict(w, r, maxProductBody, allowed)
		if !ok {
			return
		}
		delta, present, err := optInt64(raw, "delta")
		if err != nil || !present {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"missing_field", "delta is required and must be an integer")
			return
		}
		inv, err := svc.AdjustInventory(r.Context(), userID, id, int(delta))
		if err != nil {
			catalogError(w, r, log, "adjust inventory", err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

// methodMux dispatches inside one route by HTTP method, since
// main.go's methodGuard permits several methods on the same pattern
// (e.g. GET + PATCH + DELETE on /v1/products/{id}).
func methodMux(handlers map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := handlers[r.Method]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ProductsMux serves /v1/products: POST (owner create) and GET
// (public seller listing).
func ProductsMux(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	return methodMux(map[string]http.HandlerFunc{
		http.MethodPost: CreateProductHandler(pool, svc, hooks, log),
		http.MethodGet:  ListProductsHandler(svc, log),
	})
}

// ProductMux serves /v1/products/{id}: GET (public), PATCH and DELETE
// (owner-only).
func ProductMux(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	return methodMux(map[string]http.HandlerFunc{
		http.MethodGet:    GetProductHandler(svc, log),
		http.MethodPatch:  UpdateProductHandler(pool, svc, hooks, log),
		http.MethodDelete: DeleteProductHandler(pool, svc, hooks, log),
	})
}

// VariantMux serves /v1/variants/{id}: PATCH and DELETE (owner-only).
func VariantMux(pool *db.Pool, svc *Service, hooks ProductHooks, log *slog.Logger) http.HandlerFunc {
	return methodMux(map[string]http.HandlerFunc{
		http.MethodPatch:  UpdateVariantHandler(pool, svc, hooks, log),
		http.MethodDelete: DeleteVariantHandler(pool, svc, hooks, log),
	})
}

// catalogClientErr reports whether err is one of the mapped F1b
// client errors (kept beside clientErr for the new error set).
func catalogClientErr(err error) bool {
	for _, e := range []error{
		ErrNotSeller, ErrNotOwner, ErrProductNotFound,
		ErrVariantNotFound, ErrCategoryNotFound, ErrInvalidTitle,
		ErrInvalidDescription, ErrInvalidStatus, ErrInvalidPrice,
		ErrInvalidMRP, ErrSKUExists, ErrInvalidSKU,
		ErrInvalidAttributes, ErrInsufficientStock,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// catalogErrorCode maps an F1b catalog error onto its envelope code
// and HTTP status.
func catalogErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotSeller):
		return http.StatusForbidden, "seller_not_registered"
	case errors.Is(err, ErrNotOwner):
		return http.StatusForbidden, "not_owner"
	case errors.Is(err, ErrProductNotFound):
		return http.StatusNotFound, "product_not_found"
	case errors.Is(err, ErrVariantNotFound):
		return http.StatusNotFound, "variant_not_found"
	case errors.Is(err, ErrCategoryNotFound):
		return http.StatusUnprocessableEntity, "category_not_found"
	case errors.Is(err, ErrSKUExists):
		return http.StatusConflict, "sku_exists"
	case errors.Is(err, ErrInsufficientStock):
		return http.StatusConflict, "insufficient_stock"
	case errors.Is(err, ErrInvalidTitle):
		return http.StatusUnprocessableEntity, "invalid_title"
	case errors.Is(err, ErrInvalidDescription):
		return http.StatusUnprocessableEntity, "invalid_description"
	case errors.Is(err, ErrInvalidStatus):
		return http.StatusUnprocessableEntity, "invalid_status"
	case errors.Is(err, ErrInvalidPrice):
		return http.StatusUnprocessableEntity, "invalid_price"
	case errors.Is(err, ErrInvalidMRP):
		return http.StatusUnprocessableEntity, "invalid_mrp"
	case errors.Is(err, ErrInvalidSKU):
		return http.StatusUnprocessableEntity, "invalid_sku"
	case errors.Is(err, ErrInvalidAttributes):
		return http.StatusUnprocessableEntity, "invalid_attributes"
	}
	return http.StatusInternalServerError, ""
}
