// SPDX-License-Identifier: AGPL-3.0-or-later
// Unit tests for the /v1/search read path: param parsing, cursor
// codec, filter construction (incl. the numeric keyset clause), the
// Meilisearch request shape, and the HTTP handler's status contract.
// All against stub servers — no Meilisearch needed.
package search

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// searchStub answers /indexes/products/search with canned hits and
// records every request.
type searchStub struct {
	mu   sync.Mutex
	reqs []stubReq
	hits []ProductDoc
	fail bool // 500 every request
}

func (s *searchStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.reqs = append(s.reqs, stubReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		s.mu.Unlock()
		if s.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": s.hits, "query": "", "processingTimeMs": 1,
			"limit": 50, "offset": 0, "estimatedTotalHits": len(s.hits),
		})
	}
}

func (s *searchStub) requests() []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubReq(nil), s.reqs...)
}

func newSearchStubClient(t *testing.T, stub *searchStub) *Client {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "test-master-key-12345", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func testDoc(id string, updated int64, seq int64) ProductDoc {
	min := int64(49900)
	return ProductDoc{
		ID: id, Title: "Kurta " + id, Description: "Handloom",
		SellerID: "seller-1", CategorySlug: "kurtas", Status: "active",
		MinPricePaise: &min, UpdatedAtUnix: updated, SortSeq: seq,
	}
}

func TestParseSearchParamsDefaults(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/search", nil)
	p, err := ParseSearchParams(r)
	if err != nil {
		t.Fatalf("empty query = %v, want no error", err)
	}
	if p.Status != "active" || p.Limit != 20 || p.Q != "" || p.Cursor != "" {
		t.Errorf("defaults = %+v, want status=active limit=20", p)
	}
	if p.MinPrice != nil || p.MaxPrice != nil {
		t.Errorf("price filters default to nil, got %+v", p)
	}
}

func TestParseSearchParamsValidation(t *testing.T) {
	cases := []struct {
		query string
		code  string
	}{
		{"?seller_id=nope", "invalid_seller_id"},
		{"?category_slug=HAS_CAPS", "invalid_category_slug"},
		{"?category_slug=" + strings.Repeat("a", 65), "invalid_category_slug"},
		{"?status=deleted", "invalid_status"},
		{"?min_price_paise=-1", "invalid_min_price"},
		{"?min_price_paise=much", "invalid_min_price"},
		{"?max_price_paise=-1", "invalid_max_price"},
		{"?min_price_paise=200&max_price_paise=100", "invalid_price_range"},
		{"?limit=0", "invalid_limit"},
		{"?limit=51", "invalid_limit"},
		{"?limit=many", "invalid_limit"},
		{"?cursor=!!!not-base64!!!", "invalid_cursor"},
		{"?q=" + strings.Repeat("x", 201), "invalid_query"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/v1/search"+tc.query, nil)
		_, err := ParseSearchParams(r)
		pe, ok := err.(*ParamError)
		if !ok {
			t.Errorf("query %q: err type = %T, want *ParamError (err=%v)", tc.query, err, err)
			continue
		}
		if pe.Code != tc.code {
			t.Errorf("query %q: code = %q, want %q", tc.query, pe.Code, tc.code)
		}
	}
	// A fully valid query parses with every field populated.
	r := httptest.NewRequest(http.MethodGet,
		"/v1/search?q=kurta&seller_id=123e4567-e89b-12d3-a456-426614174000&category_slug=kurtas&status=draft&min_price_paise=100&max_price_paise=9000&limit=7", nil)
	p, err := ParseSearchParams(r)
	if err != nil {
		t.Fatalf("valid query = %v", err)
	}
	if p.Q != "kurta" || p.SellerID != "123e4567-e89b-12d3-a456-426614174000" ||
		p.CategorySlug != "kurtas" || p.Status != "draft" || p.Limit != 7 ||
		p.MinPrice == nil || *p.MinPrice != 100 || p.MaxPrice == nil || *p.MaxPrice != 9000 {
		t.Errorf("parsed = %+v, want all fields", p)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	cur := encodeCursor(1729000000, 42)
	c, err := decodeCursor(cur)
	if err != nil {
		t.Fatalf("decode own cursor: %v", err)
	}
	if c.U != 1729000000 || c.S != 42 {
		t.Errorf("decoded = %+v, want {1729000000 42}", c)
	}
	for _, bad := range []string{
		"!!!",
		base64.RawURLEncoding.EncodeToString([]byte("not json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"u":-1,"s":2}`)),
	} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) = nil, want error", bad)
		}
	}
}

func TestBuildFilterKeyset(t *testing.T) {
	// No cursor: plain facet filters, no range clause.
	f, err := buildFilter(SearchParams{Status: "active", Limit: 20})
	if err != nil {
		t.Fatalf("buildFilter: %v", err)
	}
	if f != `status = "active"` {
		t.Errorf("filter = %q, want just the status clause", f)
	}
	if strings.Contains(f, "updated_at_unix <") {
		t.Errorf("filter without cursor must not contain a keyset clause: %q", f)
	}
	// With cursor: the numeric keyset clause appears.
	p := SearchParams{Status: "active", Limit: 20, Cursor: encodeCursor(100, 7),
		SellerID: "123e4567-e89b-12d3-a456-426614174000", CategorySlug: "kurtas"}
	min, max := int64(100), int64(9000)
	p.MinPrice, p.MaxPrice = &min, &max
	f, err = buildFilter(p)
	if err != nil {
		t.Fatalf("buildFilter: %v", err)
	}
	for _, want := range []string{
		`status = "active"`,
		`seller_id = "123e4567-e89b-12d3-a456-426614174000"`,
		`category_slug = "kurtas"`,
		`min_price_paise >= 100`,
		`min_price_paise <= 9000`,
		`(updated_at_unix < 100 OR (updated_at_unix = 100 AND sort_seq < 7))`,
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter = %q, missing %q", f, want)
		}
	}
	// filterString is the injection backstop.
	if _, err := filterString(`a"b`); err == nil {
		t.Error("filterString with a quote must error")
	}
}

func TestSearchRequestShape(t *testing.T) {
	stub := &searchStub{hits: []ProductDoc{
		testDoc("p1", 300, 30),
		testDoc("p2", 200, 20),
		testDoc("p3", 100, 10),
	}}
	c := newSearchStubClient(t, stub)

	page, err := c.Search(testCtx(), SearchParams{Status: "active", Q: "kurta", Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Hits) != 2 {
		t.Fatalf("got %d hits, want 2 (limit+1 probe trimmed)", len(page.Hits))
	}
	if page.Hits[0].ID != "p1" || page.Hits[1].ID != "p2" {
		t.Errorf("hits order = %v, want [p1 p2]", page.Hits)
	}
	if page.NextCursor == "" {
		t.Fatal("3 hits with limit 2 must produce a next_cursor")
	}
	cur, err := decodeCursor(page.NextCursor)
	if err != nil {
		t.Fatalf("next_cursor malformed: %v", err)
	}
	if cur.U != 200 || cur.S != 20 {
		t.Errorf("cursor = %+v, want the last returned hit's sort key {200 20}", cur)
	}

	reqs := stub.requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	last := reqs[len(reqs)-1]
	if last.method != http.MethodPost || last.path != "/indexes/products/search" {
		t.Errorf("search = %s %s, want POST /indexes/products/search", last.method, last.path)
	}
	if last.auth != "Bearer test-master-key-12345" {
		t.Errorf("auth = %q, want Bearer master key", last.auth)
	}
	var body map[string]any
	if err := json.Unmarshal(last.body, &body); err != nil {
		t.Fatalf("search body not JSON: %v", err)
	}
	if body["q"] != "kurta" {
		t.Errorf("q = %v, want kurta", body["q"])
	}
	if body["limit"] != float64(3) {
		t.Errorf("limit = %v, want 3 (limit+1 probe)", body["limit"])
	}
	sort, _ := body["sort"].([]any)
	if len(sort) != 2 || sort[0] != "updated_at_unix:desc" || sort[1] != "sort_seq:desc" {
		t.Errorf("sort = %v, want [updated_at_unix:desc sort_seq:desc]", body["sort"])
	}
	filter, _ := body["filter"].(string)
	if !strings.Contains(filter, `status = "active"`) {
		t.Errorf("filter = %q, want the status gate", filter)
	}
}

func TestSearchFollowsCursor(t *testing.T) {
	stub := &searchStub{hits: []ProductDoc{testDoc("p3", 100, 10)}}
	c := newSearchStubClient(t, stub)

	page, err := c.Search(testCtx(), SearchParams{
		Status: "active", Limit: 20, Cursor: encodeCursor(200, 20),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Hits) != 1 || page.NextCursor != "" {
		t.Errorf("last page = %+v, want 1 hit and no next_cursor", page)
	}
	last := stub.requests()[len(stub.requests())-1]
	var body map[string]any
	if err := json.Unmarshal(last.body, &body); err != nil {
		t.Fatalf("search body not JSON: %v", err)
	}
	filter, _ := body["filter"].(string)
	if !strings.Contains(filter, "(updated_at_unix < 200 OR (updated_at_unix = 200 AND sort_seq < 20))") {
		t.Errorf("page-2 filter = %q, want the keyset clause", filter)
	}
}

func TestSearchDisabled(t *testing.T) {
	c, err := NewClient("http://meilisearch:7700", "", testLog())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.Search(testCtx(), SearchParams{Status: "active"}); err != ErrSearchDisabled {
		t.Errorf("disabled Search = %v, want ErrSearchDisabled", err)
	}

	// The handler 503s (with Retry-After) instead of leaking the
	// unconfigured dependency as a 500.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=kurta", nil)
	SearchHandler(c, testLog()).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled handler = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("disabled handler must send Retry-After")
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("disabled body not JSON: %v", err)
	}
	if errObj, _ := env["error"].(map[string]any); errObj["code"] != "search_unavailable" {
		t.Errorf("envelope code = %v, want search_unavailable", errObj["code"])
	}

	// nil client is the same 503 (unit-test wiring).
	rec = httptest.NewRecorder()
	SearchHandler(nil, testLog()).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil-client handler = %d, want 503", rec.Code)
	}
}

func TestSearchHandlerValidation(t *testing.T) {
	stub := &searchStub{}
	c := newSearchStubClient(t, stub)
	h := SearchHandler(c, testLog())

	for query, code := range map[string]string{
		"?cursor=bogus":                        "invalid_cursor",
		"?limit=99":                            "invalid_limit",
		"?status=exploded":                     "invalid_status",
		"?min_price_paise=9&max_price_paise=1": "invalid_price_range",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/search"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", query, rec.Code)
			continue
		}
		var env map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if errObj, _ := env["error"].(map[string]any); errObj["code"] != code {
			t.Errorf("query %q: envelope code = %v, want %q", query, errObj["code"], code)
		}
	}
}

func TestSearchHandlerUpstreamDown(t *testing.T) {
	stub := &searchStub{fail: true}
	c := newSearchStubClient(t, stub)

	rec := httptest.NewRecorder()
	SearchHandler(c, testLog()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/search?q=kurta", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("upstream-down = %d, want 503 (never 500)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("upstream-down must send Retry-After")
	}
}

func TestSearchHandlerSuccess(t *testing.T) {
	stub := &searchStub{hits: []ProductDoc{testDoc("p1", 300, 30)}}
	c := newSearchStubClient(t, stub)

	rec := httptest.NewRecorder()
	SearchHandler(c, testLog()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/search?q=kurta&limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("success = %d, want 200", rec.Code)
	}
	var page SearchPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("response not a SearchPage: %v", err)
	}
	if len(page.Hits) != 1 || page.Hits[0].ID != "p1" {
		t.Errorf("hits = %+v, want [p1]", page.Hits)
	}
	if page.NextCursor != "" {
		t.Errorf("single hit must not produce next_cursor, got %q", page.NextCursor)
	}
}

// TestEnsureIndexSettingsKeysetFields guards the settings contract
// the keyset pager depends on: sort_seq and updated_at_unix must be
// filterable (range filters), sort_seq sortable.
func TestEnsureIndexSettingsKeysetFields(t *testing.T) {
	stub := &stubMeili{}
	c := newStubClient(t, stub)
	if err := c.IndexProduct(testCtx(), &ProductDoc{ID: "p1", SortSeq: 1}); err != nil {
		t.Fatalf("IndexProduct: %v", err)
	}
	var settings map[string]any
	for _, rq := range stub.requests() {
		if rq.method == http.MethodPatch && rq.path == "/indexes/products/settings" {
			if err := json.Unmarshal(rq.body, &settings); err != nil {
				t.Fatalf("settings not JSON: %v", err)
			}
		}
	}
	if settings == nil {
		t.Fatal("no settings PATCH recorded")
	}
	contains := func(key, val string) bool {
		vals, _ := settings[key].([]any)
		for _, v := range vals {
			if v == val {
				return true
			}
		}
		return false
	}
	for _, want := range [][2]string{
		{"filterableAttributes", "sort_seq"},
		{"filterableAttributes", "updated_at_unix"},
		{"sortableAttributes", "sort_seq"},
	} {
		if !contains(want[0], want[1]) {
			t.Errorf("settings[%s] missing %q: %v", want[0], want[1], settings[want[0]])
		}
	}
	// The indexed document carries the tie-breaker.
	var docs []map[string]any
	for _, rq := range stub.requests() {
		if strings.HasSuffix(rq.path, "/documents") {
			_ = json.Unmarshal(rq.body, &docs)
		}
	}
	if len(docs) != 1 || docs[0]["sort_seq"] != float64(1) {
		t.Errorf("indexed doc = %v, want sort_seq=1", docs)
	}
}
