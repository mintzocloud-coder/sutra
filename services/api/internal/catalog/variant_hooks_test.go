// SPDX-License-Identifier: AGPL-3.0-or-later
// F2 (part 2a): variant writes must rebuild the parent product's
// search document. Gated on TEST_DATABASE_URL like the other catalog
// integration tests.
package catalog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// hookSpy records every product id the reindex hook fires for.
type hookSpy struct {
	mu  sync.Mutex
	ids []string
}

func (s *hookSpy) hooks() ProductHooks {
	return ProductHooks{AfterProductWrite: func(ctx context.Context, productID string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ids = append(s.ids, productID)
	}}
}

func (s *hookSpy) fired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// authed wraps a handler with the real auth middleware in bypass mode
// so CallerUUID resolves the test subject, exactly as in production
// (bypass stamps the subject into the request context).
func authed(t *testing.T, h http.HandlerFunc, subject string) http.Handler {
	t.Helper()
	return middleware.RequireAuth(nil, testLogger(),
		middleware.AuthOptions{Bypass: true, BypassSubject: subject})(h)
}

func TestVariantWritesFireReindexHook(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	log := testLogger()

	subject := uniqueSubject(t)
	userID := provisionUser(t, ctx, pool, subject)
	if _, err := svc.Register(ctx, userID, "Hook Store", uniqueSlug(t, "hook-store"), ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	product, err := svc.CreateProduct(ctx, userID, "Hook Kurta", "Test listing", "", "active")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}

	spy := &hookSpy{}
	hooks := spy.hooks()

	// 1. Add a variant → hook fires with the parent product id.
	addH := authed(t, AddVariantHandler(pool, svc, hooks, log), subject)
	req := httptest.NewRequest(http.MethodPost, "/v1/products/"+product.ID+"/variants",
		strings.NewReader(`{"sku":"SKU-1","price_paise":49900}`))
	req.SetPathValue("id", product.ID)
	rec := httptest.NewRecorder()
	addH.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add variant = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var variant struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &variant); err != nil || variant.ID == "" {
		t.Fatalf("add variant response not a variant: %v (%s)", err, rec.Body.String())
	}

	// 2. Update the variant → hook fires again (price edits change
	// the search document's "from ₹X").
	updateH := authed(t, VariantMux(pool, svc, hooks, log), subject)
	req = httptest.NewRequest(http.MethodPatch, "/v1/variants/"+variant.ID,
		strings.NewReader(`{"price_paise":44900}`))
	req.SetPathValue("id", variant.ID)
	rec = httptest.NewRecorder()
	updateH.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update variant = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// 3. Archive the variant → hook fires (cheapest variant may be
	// gone, so the doc must be rebuilt).
	req = httptest.NewRequest(http.MethodDelete, "/v1/variants/"+variant.ID, nil)
	req.SetPathValue("id", variant.ID)
	rec = httptest.NewRecorder()
	updateH.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive variant = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	fired := spy.fired()
	if len(fired) != 3 {
		t.Fatalf("hook fired %d times, want 3 (add/update/archive)", len(fired))
	}
	for i, id := range fired {
		if id != product.ID {
			t.Errorf("hook call %d fired for %q, want parent product %q", i, id, product.ID)
		}
	}
}

// TestVariantWriteBumpsProductUpdatedAt proves the search sort key
// stays truthful: a variant write moves products.updated_at, so the
// rebuilt document's updated_at_unix reflects the listing change.
func TestVariantWriteBumpsProductUpdatedAt(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	subject := uniqueSubject(t)
	userID := provisionUser(t, ctx, pool, subject)
	if _, err := svc.Register(ctx, userID, "Bump Store", uniqueSlug(t, "bump-store"), ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	product, err := svc.CreateProduct(ctx, userID, "Bump Kurta", "Test listing", "", "active")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	before := product.UpdatedAt

	v, err := svc.AddVariant(ctx, userID, mustUUID(t, product.ID), "SKU-B1", 1000, nil, nil, "")
	if err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	afterAdd, err := svc.GetProduct(ctx, mustUUID(t, product.ID))
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if !afterAdd.UpdatedAt.After(before) {
		t.Errorf("add variant: updated_at %v not after %v", afterAdd.UpdatedAt, before)
	}

	fifty := int64(5000)
	if _, err := svc.UpdateVariant(ctx, userID, mustUUID(t, v.ID),
		VariantPatch{PricePaise: &fifty}); err != nil {
		t.Fatalf("UpdateVariant: %v", err)
	}
	afterUpdate, err := svc.GetProduct(ctx, mustUUID(t, product.ID))
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if !afterUpdate.UpdatedAt.After(afterAdd.UpdatedAt) {
		t.Errorf("update variant: updated_at %v not after %v", afterUpdate.UpdatedAt, afterAdd.UpdatedAt)
	}
}
