// SPDX-License-Identifier: AGPL-3.0-or-later
// Unit tests for the Meilisearch client: config fail-closed behavior,
// request shapes (paths, auth header, bodies) against a stub server,
// and the disabled-client no-op contract. No Meilisearch needed.
package search

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
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// The caller is a test, not a request handler — leaking the cancel
	// past the test is harmless, but register it anyway for hygiene.
	_ = cancel
	return ctx
}

func TestNewClientDisabled(t *testing.T) {
	c, err := NewClient("http://meilisearch:7700", "", testLog())
	if err != nil {
		t.Fatalf("empty master key must disable, not error: %v", err)
	}
	if c.Enabled() {
		t.Error("client with empty master key must report disabled")
	}
	// Disabled clients no-op without touching the network.
	if err := c.IndexProduct(testCtx(), &ProductDoc{ID: "x"}); err != nil {
		t.Errorf("disabled IndexProduct = %v, want nil", err)
	}
	if err := c.DeleteProduct(testCtx(), "x"); err != nil {
		t.Errorf("disabled DeleteProduct = %v, want nil", err)
	}
}

func TestNewClientFailClosed(t *testing.T) {
	for _, bad := range []string{"://nope", "ftp://host", "", "http://"} {
		if _, err := NewClient(bad, "long-enough-test-key", testLog()); err == nil {
			t.Errorf("NewClient(%q) = nil error, want fail-closed", bad)
		}
	}
}

// stubMeili records every request the client makes.
type stubMeili struct {
	mu     sync.Mutex
	reqs   []stubReq
	failAt string // path prefix that answers 500, "" = all succeed
}

type stubReq struct {
	method string
	path   string
	auth   string
	body   []byte
}

func (s *stubMeili) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.reqs = append(s.reqs, stubReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		s.mu.Unlock()
		if s.failAt != "" && strings.HasPrefix(r.URL.Path, s.failAt) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/indexes" {
			w.WriteHeader(http.StatusConflict) // index already exists
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *stubMeili) requests() []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubReq(nil), s.reqs...)
}

func newStubClient(t *testing.T, stub *stubMeili) *Client {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-master-key-12345", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("client with a key must be enabled")
	}
	return c
}

func TestIndexProductRequestShape(t *testing.T) {
	stub := &stubMeili{}
	c := newStubClient(t, stub)

	min := int64(49900)
	doc := &ProductDoc{
		ID: "prod-1", Title: "Cotton Kurta", Description: "Handloom",
		SellerID: "seller-1", CategorySlug: "kurtas", Status: "active",
		MinPricePaise: &min, UpdatedAtUnix: 1729000000,
	}
	if err := c.IndexProduct(testCtx(), doc); err != nil {
		t.Fatalf("IndexProduct: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3 (create index 409 + settings + documents)", len(reqs))
	}
	// 1: index creation is idempotent (409 accepted).
	if reqs[0].method != http.MethodPost || reqs[0].path != "/indexes" {
		t.Errorf("req0 = %s %s, want POST /indexes", reqs[0].method, reqs[0].path)
	}
	var create map[string]string
	if err := json.Unmarshal(reqs[0].body, &create); err != nil ||
		create["uid"] != "products" || create["primaryKey"] != "id" {
		t.Errorf("create index body = %s, want uid=products primaryKey=id", reqs[0].body)
	}
	// 2: settings carry the search contract.
	if reqs[1].method != http.MethodPatch || reqs[1].path != "/indexes/products/settings" {
		t.Errorf("req1 = %s %s, want PATCH /indexes/products/settings", reqs[1].method, reqs[1].path)
	}
	var settings map[string]any
	if err := json.Unmarshal(reqs[1].body, &settings); err != nil {
		t.Fatalf("settings body not JSON: %v", err)
	}
	if _, ok := settings["typoTolerance"]; !ok {
		t.Error("settings must configure typoTolerance explicitly")
	}
	if _, ok := settings["filterableAttributes"]; !ok {
		t.Error("settings must configure filterableAttributes")
	}
	// 3: the document add.
	if reqs[2].method != http.MethodPost || reqs[2].path != "/indexes/products/documents" {
		t.Errorf("req2 = %s %s, want POST /indexes/products/documents", reqs[2].method, reqs[2].path)
	}
	var docs []map[string]any
	if err := json.Unmarshal(reqs[2].body, &docs); err != nil || len(docs) != 1 {
		t.Fatalf("documents body = %s, want 1-doc array", reqs[2].body)
	}
	if docs[0]["id"] != "prod-1" || docs[0]["title"] != "Cotton Kurta" ||
		docs[0]["min_price_paise"] != float64(49900) {
		t.Errorf("document = %v, want id/title/min_price_paise", docs[0])
	}
	// Every request carries the master key as a Bearer token.
	for i, rq := range reqs {
		if rq.auth != "Bearer test-master-key-12345" {
			t.Errorf("req%d auth = %q, want Bearer master key", i, rq.auth)
		}
	}
	// Second write skips ensure (idempotent): exactly one more request.
	before := len(stub.requests())
	if err := c.IndexProduct(testCtx(), &ProductDoc{ID: "prod-2"}); err != nil {
		t.Fatalf("second IndexProduct: %v", err)
	}
	if got := len(stub.requests()); got != before+1 {
		t.Errorf("second index made %d requests, want exactly 1 (ensure must be once)", got-before)
	}
}

func TestDeleteProductRequestShape(t *testing.T) {
	stub := &stubMeili{}
	c := newStubClient(t, stub)
	if err := c.DeleteProduct(testCtx(), "prod-9"); err != nil {
		t.Fatalf("DeleteProduct: %v", err)
	}
	reqs := stub.requests()
	last := reqs[len(reqs)-1]
	if last.method != http.MethodDelete || last.path != "/indexes/products/documents/prod-9" {
		t.Errorf("delete = %s %s, want DELETE /indexes/products/documents/prod-9",
			last.method, last.path)
	}
}

func TestIndexProductValidation(t *testing.T) {
	stub := &stubMeili{}
	c := newStubClient(t, stub)
	if err := c.IndexProduct(testCtx(), nil); err == nil {
		t.Error("nil doc must error")
	}
	if err := c.IndexProduct(testCtx(), &ProductDoc{}); err == nil {
		t.Error("empty-id doc must error")
	}
	if err := c.DeleteProduct(testCtx(), ""); err == nil {
		t.Error("empty-id delete must error")
	}
	if len(stub.requests()) != 0 {
		t.Error("validation failures must not touch the network")
	}
}

func TestEnsureFailureSurfaces(t *testing.T) {
	stub := &stubMeili{failAt: "/indexes"}
	c := newStubClient(t, stub)
	if err := c.IndexProduct(testCtx(), &ProductDoc{ID: "prod-1"}); err == nil {
		t.Error("index-create 500 must surface as an error")
	}
	// …and the next write retries ensure instead of wedging.
	stub.failAt = ""
	if err := c.IndexProduct(testCtx(), &ProductDoc{ID: "prod-1"}); err != nil {
		t.Errorf("retry after ensure failure = %v, want nil", err)
	}
}
