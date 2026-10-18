// SPDX-License-Identifier: AGPL-3.0-or-later
// Package search is the Go side of the Wave-3 product search (F2):
// a minimal stdlib-only client for the self-hosted Meilisearch CE
// service plus the product document shape the API indexes.
//
// DESIGN: indexing is best-effort and never blocks the write path.
// Product writes (create/update/archive) stay 201/200 even when the
// search cluster is down — the API's hooks log a WARN and move on.
// Reads (GET /v1/search) land in F2 part 2; this package only writes.
//
// No third-party Meilisearch SDK: the v1 REST surface we need (add
// documents, delete one document, create index, update settings) is
// four tiny HTTP calls, and a vendored SDK would be a bigger API to
// audit than the protocol itself.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IndexUID is the Meilisearch index holding sellable listings. One
// index for the whole marketplace (seller scoping is a filter, not an
// index split — filters keep the index count constant as sellers grow).
const IndexUID = "products"

// indexTimeout bounds one Meilisearch call. The API invokes the client
// from a detached goroutine with its own 15s deadline; the client
// timeout is the inner bound so a hung cluster cannot pile up writers.
const indexTimeout = 10 * time.Second

// ProductDoc is the search document for one product. Price lives on
// variants, so the doc carries the cheapest ACTIVE variant's price —
// the number the storefront's "from ₹X" badge shows. Only ACTIVE
// products are indexed; drafts and archived listings are deleted from
// the index (F2 part 2's /v1/search filters status=active as a
// defense-in-depth second gate).
type ProductDoc struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	SellerID      string `json:"seller_id"`
	CategorySlug  string `json:"category_slug,omitempty"`
	Status        string `json:"status"`
	MinPricePaise *int64 `json:"min_price_paise,omitempty"`
	UpdatedAtUnix int64  `json:"updated_at_unix"`
}

// Client talks to one Meilisearch instance. The zero client is never
// valid — build it with NewClient.
type Client struct {
	base    string // e.g. http://meilisearch:7700, no trailing slash
	auth    string // full "Bearer <master key>" value, empty when disabled
	http    *http.Client
	log     *slog.Logger
	disable bool

	ensureMu sync.Mutex
	ensured  bool // index + settings confirmed present
}

// NewClient builds the search client. An empty master key DISABLES
// search (dev default — indexing calls become no-ops and a loud WARN
// is logged). A master key with a malformed URL is a configuration
// bug: fail closed so operators notice instead of silently dropping
// every index write.
func NewClient(baseURL, masterKey string, log *slog.Logger) (*Client, error) {
	if masterKey == "" {
		log.Warn("search: no SEARCH_MASTER_KEY configured — product search indexing disabled (products will not appear in /v1/search)")
		return &Client{disable: true, log: log}, nil
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("search: invalid SEARCH_URL %q: must be an http(s) URL", baseURL)
	}
	return &Client{
		base: u.String(),
		auth: "Bearer " + masterKey,
		http: &http.Client{Timeout: indexTimeout},
		log:  log,
	}, nil
}

// Enabled reports whether search is configured. Disabled clients
// no-op every method, so callers can skip the branch entirely.
func (c *Client) Enabled() bool { return !c.disable }

// IndexProduct upserts one product document. Async by design:
// Meilisearch returns 202 with a task id and indexes in the
// background (sub-second at our scale); the write path never waits
// for task completion. Only ACTIVE products should reach here — the
// caller deletes drafts/archived listings via DeleteProduct.
func (c *Client) IndexProduct(ctx context.Context, doc *ProductDoc) error {
	if c.disable {
		return nil
	}
	if doc == nil || doc.ID == "" {
		return fmt.Errorf("search: cannot index product with empty id")
	}
	if err := c.ensureIndex(ctx); err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost,
		"/indexes/"+IndexUID+"/documents", []any{doc})
}

// DeleteProduct removes one document from the index. Idempotent —
// deleting a missing document is a no-op on the Meilisearch side.
func (c *Client) DeleteProduct(ctx context.Context, productID string) error {
	if c.disable {
		return nil
	}
	if productID == "" {
		return fmt.Errorf("search: cannot delete product with empty id")
	}
	if err := c.ensureIndex(ctx); err != nil {
		return err
	}
	return c.call(ctx, http.MethodDelete,
		"/indexes/"+IndexUID+"/documents/"+url.PathEscape(productID), nil)
}

// ensureIndex creates the index (idempotent — Meilisearch 409s when it
// exists, which we accept) and applies the ranking/search settings.
// Runs once per client lifetime; a failed attempt retries on the next
// write instead of wedging the client forever.
func (c *Client) ensureIndex(ctx context.Context) error {
	c.ensureMu.Lock()
	defer c.ensureMu.Unlock()
	if c.ensured {
		return nil
	}
	if err := c.callAllow(ctx, http.MethodPost, "/indexes",
		map[string]string{"uid": IndexUID, "primaryKey": "id"},
		http.StatusAccepted, http.StatusConflict); err != nil {
		return fmt.Errorf("search: create index: %w", err)
	}
	settings := map[string]any{
		// Title outranks description, always.
		"searchableAttributes": []string{"title", "description"},
		// F2 part 2's /v1/search filters + sorts on these.
		"filterableAttributes": []string{"seller_id", "category_slug", "status", "min_price_paise"},
		"sortableAttributes":   []string{"min_price_paise", "updated_at_unix"},
		// Explicit typo tolerance: the storefront's killer feature for
		// Indic-script transliteration typos ("kurta" vs "kurtha").
		// Defaults are sane; the block documents the intent for ops.
		"typoTolerance": map[string]any{
			"enabled": true,
			"minWordSizeForTypos": map[string]int{
				"oneTypo":  4,
				"twoTypos": 8,
			},
		},
	}
	if err := c.callAllow(ctx, http.MethodPatch,
		"/indexes/"+IndexUID+"/settings", settings, http.StatusAccepted); err != nil {
		return fmt.Errorf("search: apply settings: %w", err)
	}
	c.ensured = true
	return nil
}

// call performs one Meilisearch request, accepting 200/201/202.
// Non-2xx responses are errors (the body is capped before reading so
// a misbehaving cluster cannot OOM the API).
func (c *Client) call(ctx context.Context, method, path string, body any) error {
	return c.callAllow(ctx, method, path, body,
		http.StatusOK, http.StatusCreated, http.StatusAccepted)
}

// callAllow is call with an explicit accepted-status set (index
// creation legitimately 409s when the index already exists).
func (c *Client) callAllow(ctx context.Context, method, path string, body any, accept ...int) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("search: marshal request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("search: build request: %w", err)
	}
	req.Header.Set("Authorization", c.auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("search: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	for _, s := range accept {
		if resp.StatusCode == s {
			return nil
		}
	}
	return fmt.Errorf("search: %s %s: unexpected status %d", method, path, resp.StatusCode)
}
