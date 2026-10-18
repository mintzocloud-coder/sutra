<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Recommendations v1 — home-feed ranking (E5)

Non-ML ranking for `GET /v1/feed/home`. The explicit design goal: a
baseline so legible and well-tested that any future ML ranker has to
*prove* it beats this, not just replace it.

## The v1 formula

For each item on a home-feed page, at read time (Go, no new infra):

```
score = w_recency · recency(age) + w_graph · graph(followers) + w_eng · engagement(likes24h, age)
```

| Term | Definition | Why this shape |
|---|---|---|
| `recency(age)` | `2^(-ageHours / 12)` — exponential decay, 12h half-life | A 12h-old post scores half a fresh one. Matches the Indian social daypart (morning ↔ evening scroll). Exponential, not linear: the *first* hours matter most. |
| `graph(followers)` | `log10(1+followers) / log10(1+100000)`, capped at 1 | Log scale is the anti-incumbent property: 0→100 followers (≈0.40) matters far more than 100k→200k. 100k ("established creator") normalizes to 1. Small creators are boosted, never zeroed. |
| `engagement(likes24h, age)` | `min( (likes24h / clamp(ageHours,1,24)) / 10 , 1)` | *Velocity*, not volume: recent likes per hour, capped at 10/h ("genuinely hot"). The denominator clamps to [1h, 24h] so a minutes-old post with 2 likes doesn't score infinite velocity, and old posts aren't punished for age twice. |

Weights (env-tunable, normalized to sum 1):

| Env | Default | Meaning |
|---|---|---|
| `REC_W_RECENCY` | `0.5` | Recency-first: a young network should surface what's happening *now* |
| `REC_W_GRAPH` | `0.2` | Follow-graph boost is the *smallest* term — deliberately, so incumbents don't entrench |
| `REC_W_ENGAGEMENT` | `0.3` | Engagement velocity second: hot posts rise fast |

All three terms live in [0,1], so the score does too. Zero data
degrades gracefully: a post with no likes and an author with no
followers scores purely on recency — never an error, never NaN.

## Where it runs

`internal/feed/rank.go`: `RankScore` is a **pure function** —
`(createdAt, now, signals, weights) → score`, no I/O, no clock, no
randomness. `RankItems` reorders one feed page by score descending;
exact ties fall back to the pre-rank (time, id) order, so ties
reproduce the old ordering deterministically.

Signals are batch-loaded per page — exactly two queries, never
per-item (`engagement.Signals` for like counts, `engagement.AuthorFollowerCounts`
for follower counts). Posts with no likes / authors with no followers
are simply absent from the maps: absence means zero.

**Pagination contract (important):** v1 is a *per-page rerank*. The
feed still merges the push/pull streams in (time, id) order, and
cursors track *stream positions*, not rank positions. That's what
keeps pagination stable across reranks. The documented cost: boundary
effects — a hot post on page 2 can't outrank page 1's items on the
same screen. Score-ordered cursors are a #19-era upgrade (below).

## Data: the like primitive

Likes are the only engagement signal in v1 (`internal/engagement`,
migration `0007_likes.sql`):

- Composite PK `(post_id, user_id)` — a user likes a post at most
  once; like is idempotent by construction (`ON CONFLICT DO NOTHING`
  + `RETURNING`, exactly like the chat service's message dedupe).
- `POST /v1/posts/{id}/like` → 201 new / 200 retry; `DELETE` → 200
  with the count (idempotent "ensure absent"; unlike targets unknown
  posts are no-ops, like un-follows).
- FKs cascade: deleting a post removes its likes.
- `Signals` counts total + trailing-24h likes per post in one
  `GROUP BY` — the velocity window.

Like-spam / brigading guards are E6 (Wave-2 hardening): per-user like
velocity caps, mirroring the post/follow caps from D3/D6.

## What's deliberately NOT here

- **No embeddings, no ML.** Issue #19 (pgvector embeddings pipeline)
  is reserved for Hacktoberfest contributors — the worker must not
  implement it. E5 stays non-ML by rule.
- **No view/impression tracking.** Views would strengthen the
  velocity term (views are denser than likes), but impression logging
  is a privacy + storage decision for Wave 2, not a silent addition.
- **No personalization.** v1 scores the same for every viewer of the
  same page. Per-user taste is the #19 upgrade path (below).
- **No `liked_by_me` / `like_count` on Post yet.** Ranking only
  reorders; the Post shape is unchanged. Client-visible counts arrive
  with the like-button UI contract in Wave 2.

## #19 integration plan (when embeddings land)

The embeddings pipeline plugs into this architecture without
rewriting it:

1. **Candidate generation (replaces nothing yet):** pgvector ANN
   search over post embeddings becomes a *third* feed source next to
   the push stream and the celebrity pull — "posts like the ones you
   engage with", keyset-paged on similarity.
2. **Personalization term:** `w_taste · taste(viewer, post)` joins
   the formula as a fourth term — cosine similarity between the
   viewer's taste vector and the post embedding, normalized to [0,1]
   like the others. Weights stay env-tunable; v1 terms remain as the
   cold-start fallback for users/posts with no embedding history.
3. **Evaluation gate:** the ML ranker must beat v1 on a held-out
   engagement metric (defined in E6's eval harness) before it ships —
   v1 is the baseline to beat, with its deterministic tests as the
   regression suite.

Until #19 lands, this document's formula *is* the ranker.
