// SPDX-License-Identifier: AGPL-3.0-or-later
// F2 (part 2a): the read half of product search — GET /v1/search.
//
// The proxy translates query params into one Meilisearch
// POST /indexes/products/search call and returns keyset-paged
// results. Paging is a TRUE keyset on (updated_at_unix, sort_seq),
// both numbers: Meilisearch's range filters (<, <=, >, >=) work on
// numbers only (official data-types doc), which is exactly why the
// catalog carries products.search_seq instead of using the uuid id
// as the tie-breaker. Cursors are opaque base64url JSON; the client
// must treat them as black boxes.
//
// Sort is fixed to updated_at desc, sort_seq desc — a storefront's
// "newest listings" browse order. Relevance ranking comes from
// Meilisearch's default ranking rules on the query text; the explicit
// sort only orders equal-relevance… no: when `sort` is set,
// Meilisearch applies it AFTER the ranking rules, so results stay
// relevance-ordered first, newest-first within equal relevance.
// (That is the documented ranking-rule order: sort is one of the 7
// default rules, applied after words/typo/proximity/attribute.)
package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
)

// Search statuses the proxy accepts. Default is active — drafts and
// archived listings leave the index on write, so this is a
// defense-in-depth second gate, not the primary one.
var validSearchStatuses = map[string]bool{
	"active": true, "draft": true, "archived": true,
}

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
	maxQueryRunes      = 200
	maxSlugRunes       = 64
)

// ParamError is a 400-class request problem with a stable code for
// the JSON error envelope.
type ParamError struct {
	Code    string
	Message string
}

func (e *ParamError) Error() string { return e.Code + ": " + e.Message }

// SearchParams is the validated form of GET /v1/search's query
// string. Pointers for optional numeric filters: nil = unfiltered.
type SearchParams struct {
	Q            string
	SellerID     string
	CategorySlug string
	Status       string
	MinPrice     *int64
	MaxPrice     *int64
	Limit        int
	Cursor       string // opaque; "" = first page
}

// searchCursor is the decoded form of the opaque cursor: the sort
// key of the last hit on the previous page.
type searchCursor struct {
	U int64 `json:"u"` // updated_at_unix
	S int64 `json:"s"` // sort_seq
}

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ParseSearchParams validates the query string. Unknown params are
// ignored (forward-compatible); malformed known ones are 400s.
func ParseSearchParams(r *http.Request) (SearchParams, error) {
	q := r.URL.Query()
	p := SearchParams{Status: "active", Limit: defaultSearchLimit}

	if v := strings.TrimSpace(q.Get("q")); v != "" {
		if utf8.RuneCountInString(v) > maxQueryRunes {
			return p, &ParamError{"invalid_query", "q must be at most 200 characters"}
		}
		p.Q = v
	}
	if v := strings.TrimSpace(q.Get("seller_id")); v != "" {
		var id pgtype.UUID
		if err := id.Scan(v); err != nil {
			return p, &ParamError{"invalid_seller_id", "seller_id must be a UUID"}
		}
		p.SellerID = v
	}
	if v := strings.TrimSpace(q.Get("category_slug")); v != "" {
		if !slugPattern.MatchString(v) {
			return p, &ParamError{"invalid_category_slug", "category_slug must match [a-z0-9-]{1,64}"}
		}
		p.CategorySlug = v
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		if !validSearchStatuses[v] {
			return p, &ParamError{"invalid_status", "status must be active, draft or archived"}
		}
		p.Status = v
	}
	if v := strings.TrimSpace(q.Get("min_price_paise")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return p, &ParamError{"invalid_min_price", "min_price_paise must be a non-negative integer"}
		}
		p.MinPrice = &n
	}
	if v := strings.TrimSpace(q.Get("max_price_paise")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return p, &ParamError{"invalid_max_price", "max_price_paise must be a non-negative integer"}
		}
		p.MaxPrice = &n
	}
	if p.MinPrice != nil && p.MaxPrice != nil && *p.MinPrice > *p.MaxPrice {
		return p, &ParamError{"invalid_price_range", "min_price_paise must not exceed max_price_paise"}
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSearchLimit {
			return p, &ParamError{"invalid_limit", "limit must be an integer 1..50"}
		}
		p.Limit = n
	}
	if v := strings.TrimSpace(q.Get("cursor")); v != "" {
		if _, err := decodeCursor(v); err != nil {
			return p, &ParamError{"invalid_cursor", "cursor is malformed; restart from the first page"}
		}
		p.Cursor = v
	}
	return p, nil
}

// encodeCursor serializes the sort key of the last hit on a page.
func encodeCursor(u, s int64) string {
	raw, _ := json.Marshal(searchCursor{U: u, S: s})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor the proxy issued. Anything else is an
// error — cursors are never constructed by clients.
func decodeCursor(cursor string) (searchCursor, error) {
	var c searchCursor
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.U < 0 || c.S < 0 {
		return c, fmt.Errorf("negative cursor component")
	}
	return c, nil
}

// filterString escapes one interpolated filter value. Caller-side
// validation already restricts the charset (uuids, [a-z0-9-] slugs);
// this is the backstop that keeps a future caller from injecting
// filter syntax.
func filterString(v string) (string, error) {
	if strings.ContainsAny(v, `"\\`) {
		return "", fmt.Errorf("unsafe filter value")
	}
	return `"` + v + `"`, nil
}

// buildFilter assembles the Meilisearch filter expression: the user's
// facet filters ANDed with the keyset clause for the cursor (the
// "page 2 starts strictly after the last hit of page 1" condition).
func buildFilter(p SearchParams) (string, error) {
	clauses := []string{}
	status, err := filterString(p.Status)
	if err != nil {
		return "", err
	}
	clauses = append(clauses, "status = "+status)
	if p.SellerID != "" {
		v, err := filterString(p.SellerID)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, "seller_id = "+v)
	}
	if p.CategorySlug != "" {
		v, err := filterString(p.CategorySlug)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, "category_slug = "+v)
	}
	if p.MinPrice != nil {
		clauses = append(clauses, fmt.Sprintf("min_price_paise >= %d", *p.MinPrice))
	}
	if p.MaxPrice != nil {
		clauses = append(clauses, fmt.Sprintf("min_price_paise <= %d", *p.MaxPrice))
	}
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return "", err
		}
		// Keyset on (updated_at_unix desc, sort_seq desc): everything
		// strictly after the cursor's sort key.
		clauses = append(clauses, fmt.Sprintf(
			"(updated_at_unix < %d OR (updated_at_unix = %d AND sort_seq < %d))",
			c.U, c.U, c.S))
	}
	return strings.Join(clauses, " AND "), nil
}

// SearchPage is one page of results. NextCursor is empty on the last
// page — clients stop when it is absent, never by counting hits.
type SearchPage struct {
	Hits       []ProductDoc `json:"hits"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type meiliSearchResponse struct {
	Hits []ProductDoc `json:"hits"`
}

// searchRequestTimeout bounds the read path. Search is synchronous
// (unlike indexing), but a hung cluster must still fail fast into a
// 503 instead of holding the request open.
const searchRequestTimeout = 5 * time.Second

// Search runs one product search against Meilisearch. Disabled
// clients return ErrSearchDisabled — the handler maps it to 503.
func (c *Client) Search(ctx context.Context, p SearchParams) (*SearchPage, error) {
	if c.disable {
		return nil, ErrSearchDisabled
	}
	filter, err := buildFilter(p)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"q":      p.Q,
		"filter": filter,
		"sort":   []string{"updated_at_unix:desc", "sort_seq:desc"},
		"limit":  p.Limit + 1, // one extra to detect the next page
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, fmt.Errorf("search: encode request: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, searchRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost,
		c.base+"/indexes/"+IndexUID+"/search", &buf)
	if err != nil {
		return nil, fmt.Errorf("search: build request: %w", err)
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: query failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search: unexpected status %d", resp.StatusCode)
	}
	var out meiliSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("search: decode response: %w", err)
	}
	page := &SearchPage{Hits: out.Hits}
	if len(page.Hits) > p.Limit {
		last := page.Hits[p.Limit-1]
		page.NextCursor = encodeCursor(last.UpdatedAtUnix, last.SortSeq)
		page.Hits = page.Hits[:p.Limit]
	}
	return page, nil
}

// ErrSearchDisabled is returned when search was never configured
// (empty SEARCH_MASTER_KEY). The handler turns it into a 503 with
// Retry-After — the same contract as the AI proxy's disabled mode.
var ErrSearchDisabled = fmt.Errorf("search: product search is not configured")

// SearchHandler serves GET /v1/search. Upstream failures are 503s
// (never 500s): search is a dependency, not a bug, when Meilisearch
// is down.
func SearchHandler(client *Client, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if client == nil || !client.Enabled() {
			w.Header().Set("Retry-After", "60")
			middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
				"search_unavailable", "product search is not configured")
			return
		}
		params, err := ParseSearchParams(r)
		if err != nil {
			var pe *ParamError
			code, msg := "invalid_request", err.Error()
			if errors.As(err, &pe) {
				code, msg = pe.Code, pe.Message
			}
			middleware.WriteJSONError(w, r, http.StatusBadRequest, code, msg)
			return
		}
		page, err := client.Search(r.Context(), params)
		if err != nil {
			log.Warn("search: query failed", "q", params.Q, "err", err)
			w.Header().Set("Retry-After", "30")
			middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
				"search_unavailable", "product search is temporarily unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(page)
	}
}
