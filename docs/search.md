<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Product search (F2)

Storefront product search over the commerce catalog, backed by
**Meilisearch CE v1.38.0** (pinned image in `docker-compose.yml`,
MIT license — deliberately the community edition, not the cloud
service: the data never leaves our infrastructure, there is no
per-search meter, and the 0%-commission model can't fund one).

Two halves, different contracts:

- **Write half** — indexing is best-effort and *never* blocks the
  catalog write path. A product create/update/archive stays
  201/200 even when Meilisearch is down: the API logs a WARN and
  moves on. Search is a view over the catalog, not a second source
  of truth.
- **Read half** — `GET /v1/search` is a synchronous, dependency-aware
  proxy. When Meilisearch is unreachable the handler 503s
  (`search_unavailable` + `Retry-After`) — never a 500: a down
  search cluster is a dependency outage, not a bug.

## The index

| Setting | Value | Why |
|---|---|---|
| Index UID | `products` | One index for the whole marketplace; seller scoping is a **filter**, not an index split — the index count stays constant as sellers grow |
| Primary key | `id` (product uuid) | Upserts and deletes are one document call |
| Searchable | `title`, then `description` | Title outranks description, always |
| Filterable | `seller_id`, `category_slug`, `status`, `min_price_paise`, `updated_at_unix`, `sort_seq` | Every facet + the keyset cursor's range clause |
| Sortable | `min_price_paise`, `updated_at_unix`, `sort_seq` | Price sorts for a later "price: low→high" storefront control |
| Typo tolerance | enabled; oneTypo ≥ 4 chars, twoTypos ≥ 8 chars | Explicit (not default-inherited) — transliteration typos are the storefront's killer case (`kurta` vs `kurtha`, `saree` vs `sari`) |

The index is created **once per API lifetime** (`ensureIndex`,
409-tolerant create + settings patch): a failed attempt retries on
the next product write instead of wedging the client forever.

## The document

One document per product (`internal/search.ProductDoc`):

| Field | Content | Notes |
|---|---|---|
| `id` | product uuid | Primary key |
| `title`, `description` | catalog row | Searchable |
| `seller_id` | seller uuid | Storefront filter |
| `category_slug` | e.g. `audio` | From the category tree; omitted when uncategorized |
| `status` | `active` / `draft` / `archived` | Only `active` products are *indexed* |
| `min_price_paise` | cheapest **active** variant's price | The storefront's "from ₹X" badge number; omitted when there are no active variants |
| `updated_at_unix` | product `updated_at` as unix seconds | One half of the keyset sort |
| `sort_seq` | `products.search_seq` (identity column) | Numeric keyset tie-breaker (see below) |

### Lifecycle: active → indexed, everything else → deleted

- Product write (create / update / archive) → `ProductHooks.AfterProductWrite`
  fires **after the DB commit**, in a detached goroutine with a 15s
  deadline: build the document → `active` ⇒ upsert document,
  `draft`/`archived` ⇒ delete document.
- **Variant writes rebuild the parent product's document too**
  (add/update/archive variant → `products.updated_at` is bumped in
  the same write and the hook fires with the parent product id) —
  so a price change or variant archive is reflected in
  `min_price_paise` without a separate sync job.
- The `hook` is only armed when the search client is enabled
  (non-empty `SEARCH_MASTER_KEY`); the zero hooks value disables
  indexing for unit tests.

## The read path: `GET /v1/search`

Behind `RequireAuth` like the rest of `/v1/`. The proxy translates
query params into one Meilisearch `POST /indexes/products/search`
call (5s bound) and returns a keyset page.

### Parameters

| Param | Type | Notes |
|---|---|---|
| `q` | string ≤ 200 chars | Optional; empty = browse order (newest first within relevance) |
| `seller_id` | uuid | Filter: one seller's storefront |
| `category_slug` | `[a-z0-9-]{1,64}` | Filter: category browse |
| `status` | `active` / `draft` / `archived` | Default `active` — a **defense-in-depth second gate**: drafts/archived leave the index on write, but a stale doc can never leak through the proxy with a forged status filter |
| `min_price_paise`, `max_price_paise` | non-negative int | Price band over `min_price_paise`; min > max is 400 |
| `limit` | 1–50 | Default 20; out of range is 400 |
| `cursor` | opaque base64url | From the previous page's `next_cursor`; malformed is 400 |

### Keyset paging on numbers, not uuids

Pages are ordered `updated_at_unix desc, sort_seq desc`, and the
cursor is the previous page's last hit's sort key. The tie-breaker
is the **numeric** `search_seq` identity column (migration 0009)
because Meilisearch's range filters (`<`, `<=`, `>`, `>=`) work on
**numbers only** (official data-types doc) — the uuid PK cannot
serve as the cursor's tie-breaker. Clients must treat cursors as
opaque black boxes; stop when `next_cursor` is absent, never by
counting hits.

Sort is fixed (no client-controlled sort in Wave 3): Meilisearch
applies its default ranking rules on the query text *first* (words,
typo, proximity, attribute) and the explicit sort orders within
equal relevance — newest listings first among equally relevant
hits.

### Error codes (stable envelope codes)

| Code | HTTP | Meaning |
|---|---|---|
| `invalid_query` / `invalid_seller_id` / `invalid_category_slug` / `invalid_status` / `invalid_min_price` / `invalid_max_price` / `invalid_price_range` / `invalid_limit` / `invalid_cursor` | 400 | Malformed param — message says the grammar |
| *(auth/rate-limit envelope codes)* | 401 / 429 | Standard A2/A5 paths |
| `search_unavailable` | 503 + `Retry-After` | Search disabled (no key) **or** upstream down/timed out. The handler cannot tell "never configured" from "cluster down" at request time in a way clients care about — both are "search is not available right now". |

## Failure modes

| Situation | Write path | Read path |
|---|---|---|
| `SEARCH_MASTER_KEY` empty | Indexing disabled (zero hooks); loud startup WARN | 503 `search_unavailable` + `Retry-After: 60` |
| Key set, bad `SEARCH_URL` | — (boot refused, fail-closed: "invalid search config") | — |
| Meilisearch down mid-write | Hook WARNs; the 201/200 is unaffected | 503 + `Retry-After: 30` |
| Index/settings call fails | `ensureIndex` retries on the next product write (never wedged) | N/A |
| Indexing slower than expected | Products appear once Meilisearch processes the async task (sub-second at our scale); the smoke test **polls** rather than asserting instantly | N/A |

## Configuration

| Var | Default | Purpose |
|---|---|---|
| `SEARCH_URL` | `http://meilisearch:7700` | API → Meilisearch base URL (Go, `internal/config.Load`) |
| `SEARCH_MASTER_KEY` | *(empty = search disabled)* | Bearer auth for the v1 REST calls; **must equal** `MEILI_MASTER_KEY`; never in the repo |
| `MEILI_MASTER_KEY` | `sutra-dev-master-key` (compose dev default) | Meilisearch's own auth; **production must override** (see `docs/production.md`) |

The API constructs its client at boot: bad URL + key set ⇒
`exit 1` (fail-closed — a misconfigured search must be noticed,
not silently dropped). Disabled ⇒ every indexing call is a no-op
and the service runs searchless.

## Ops notes

- **Backup:** snapshot the `meili_data` named volume with your
  volume backups (postgres stays the source of truth — the index
  is fully rebuildable).
- **Rebuild:** delete the index (`DELETE /indexes/products` with the
  master key) → the next product write re-runs `ensureIndex` and
  re-adds documents as writes flow. For a full catalog replay,
  re-touch products (e.g. a no-op status→status PATCH per row) —
  each write re-indexes its document.
- **Wave 3 out of scope:** facet counts, synonyms, custom ranking
  rules, client-controlled sort, multi-index/storefront-localized
  indexes. All are one settings patch away when the storefront
  asks for them.

See `internal/search/search.go` (client + settings),
`internal/search/query.go` (proxy + paging),
`internal/catalog/index.go` (document builder + hooks).
