<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Home feed: hybrid fan-out / fan-in (D3b)

The home timeline is the classic social-feed scaling problem: a celebrity's
single post must reach millions of followers without a million-row write,
while a normal user's post should appear in followers' feeds instantly.
Sutra uses the industry-standard **hybrid model** (the same shape Twitter/X
and Instagram run): fan-out-on-write for normal authors, fan-in-on-read for
celebrities, merged at read time.

## The two paths

**Push path (fan-out-on-write).** When a post is created, the API fans it
out into one Valkey sorted set per recipient:

- Key: `feed:home:{userID}` — the user's home timeline.
- Member: the post id. Score: `created_at` as **unix microseconds**
  (float64). 1.8e12 fits exactly in a float64 mantissa (2^53 ≈ 9e15), so
  ordering is lossless — unix *nanoseconds* would lose precision and
  corrupt timeline order.
- Recipients: every follower of the author **plus the author themselves**
  (your own posts appear in your own feed).
- Each timeline is trimmed to the newest `FEED_TIMELINE_MAX` entries
  (default 2000 — nobody scrolls deeper; the cap bounds Valkey memory per
  user) and carries a 30-day TTL refreshed on every write, so abandoned
  accounts age out of memory while active users keep theirs.

**Pull path (fan-in-on-read).** Authors with **more** than `FANOUT_THRESHOLD`
followers (default 10,000, env `FANOUT_THRESHOLD`) are "celebrities": their
posts are **never** fanned out — the write amplification of a viral author
is bounded at zero. Instead, `GET /v1/feed/home` reads their recent posts
from PostgreSQL at request time (keyset on `(created_at, id)`, never
OFFSET — the same pagination discipline as every other listing).

**Merge.** The read path k-way merges the two newest-first streams over
`(time, id)`, dedupes by post id (a post can legitimately appear in *both*
streams when an author crosses the threshold between write and read), and
pages with per-source cursors. A stream is only marked done when the merge
consumed everything it fetched: if the merge starves one stream on a page
(takes all `limit` entries from the other), the starved stream resumes on
the next page instead of being dropped — no post is silently skipped.

**Rehydration (source of truth).** Pulled posts arrive from PostgreSQL
already hydrated. Pushed entries are post *ids*; they resolve against
PostgreSQL in one batch query. Posts missing from the DB — deleted between
fan-out and read, or orphaned by a lost unfan-out — are **dropped**: the
feed shows a hole, never a ghost post.

## Delete propagation

Every fan-out records its recipients in `feed:postfans:{postID}` (a set
with the same TTL). Deleting a post `ZREM`s it from exactly those
timelines and drops the set. Celebrity posts need no propagation (nothing
was fanned out). The rehydration step is the backstop: even if the
unfan-out is lost, the read path self-heals.

## Failure semantics (deliberate, documented)

| Failure | Behavior |
|---|---|
| Fan-out fails on `POST /v1/posts` | **Compensating rollback**: the post is deleted again and the request fails `503 feed_unavailable` (+ `Retry-After`). A retry is safe — the first attempt never became visible. **201 always means "visible in home timelines."** |
| Unfan-out fails on `DELETE /v1/posts/{id}` | Best-effort: logged, delete still 204s. The read path drops missing posts, so the feed self-heals. |
| Valkey down on `GET /v1/feed/home` | `503 feed_unavailable` (+ `Retry-After`). Author timelines and post CRUD keep working — they are pure PostgreSQL. |
| Valkey down at API boot | **Fail closed**: the API refuses to start (exit 1) rather than serve a silently broken feed. Mid-run outages degrade to 503s and self-heal. |

## Cursors

The `next_cursor` is an opaque base64url JSON blob with per-source resume
points:

- `p`: the pushed stream resumes strictly after `(score, member)` in
  `(score DESC, member DESC)` order. Same-score ties (two posts in one
  microsecond — essentially impossible, but handled) paginate by member
  DESC so the resume cursor can never skip them.
- `c`: the pull stream resumes with the standard `(created_at, id)` keyset.
- `pd` / `cd`: a stream marked done is skipped on the next page.
- Both done → no `next_cursor` (end of feed).

Malformed cursors are `400 invalid_cursor`, never a store error.

## Client

The Go API uses `github.com/redis/go-redis/v9` — it speaks RESP, which
Valkey implements, so no Valkey-specific client is needed. Pinned at
**v9.16.0**: the newest go-redis whose `go` directive still fits the
service's Go 1.22 toolchain (v9.19.0+ requires go ≥ 1.24). The choice was
validated against a live server: the full fan-out / merge / delete /
pagination suite runs against real Valkey semantics in CI-gated
integration tests (`internal/feed/feed_test.go`, gated on
`TEST_DATABASE_URL` + `REDIS_TEST_ADDR`).

## Tuning

| Env | Default | Meaning |
|---|---|---|
| `VALKEY_ADDR` | `valkey:6379` | Timeline store address. |
| `FANOUT_THRESHOLD` | `10000` | Authors with *more* followers than this go pull-on-read. Lower it if p99 post-creation latency climbs (fan-out is O(followers)); raise it if home-feed read latency climbs (pull path is a per-read query). |
| `FEED_TIMELINE_MAX` | `2000` | Entries kept per home timeline. |

The 30-day timeline TTL is a code constant (`homeTTL`): active users
refresh it on every fan-out; it is not env-configurable because changing
it has no operational value — abandoned timelines aging out a few days
earlier or later changes nothing about correctness.

## What this is not (Wave 2)

- No ranking/relevance model: the feed is strictly reverse-chronological.
  Ranking is a Wave 2 product decision, not a Wave 1 omission.
- No notification persistence for new posts (see D5 for the realtime
  event contract; the inbox is Wave 2).
- E2E encryption is Wave 2 and is never promised anywhere in this stack.
