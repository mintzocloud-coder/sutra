// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/engagement"
)

// Home-feed ranking v1 (E5): recency decay + follow-graph boost +
// engagement velocity, computed in Go at read time. Non-ML by design —
// this is the documented baseline that issue #19's vector embeddings
// will replace/extend (see docs/recsys.md).
//
// The scorer is a PURE function: same (createdAt, now, signals,
// weights) → same score, always. That is what makes ranking tests
// deterministic and what lets clients reason about ordering. `now` is
// an explicit parameter precisely so tests never depend on the clock.
//
// v1 is a per-page rerank: HomeFeed merges the push/pull streams in
// (time, id) order (cursors depend on that), then RankItems reorders
// the returned page by score. Pagination stays stable because cursors
// track stream positions, not rank positions; the documented cost is
// boundary effects (a hot post on page 2 can't outrank page 1's items
// on the same screen). Score-ordered cursors arrive with #19.

// RecencyHalfLifeHours is the exponential half-life of the recency
// term: a 12-hour-old post scores half a fresh one, all else equal.
// 12h matches the Indian social daypart (morning scroll ↔ evening
// scroll); tune with REC_W_* weights, not by editing this.
const RecencyHalfLifeHours = 12

// GraphRefFollowers normalizes the follow-graph boost: an author with
// this many followers scores 1.0 on the graph term. 100k is "established
// creator" scale on an India-first network; the log scale means 100
// followers still beats 0 (0.33), so small creators are never zeroed.
const GraphRefFollowers = 100_000

// EngagementRefPerHour normalizes the velocity term: a post earning
// this many likes per hour scores 1.0. 10 likes/hour is genuinely hot
// for a young network; the cap keeps one viral post from flattening
// everything else on the page.
const EngagementRefPerHour = 10

// VelocityWindowHours bounds the age used for the per-hour rate: a
// 3-day-old post with 6 recent likes is 6/72 per hour only if we let
// the denominator grow unbounded — but "velocity" should measure
// RECENT heat, so the window caps at 24h (the same window Signals
// measures).
const VelocityWindowHours = 24

// RankWeights are the three term weights. They are normalized to sum
// 1 (see Normalize) so tuning is "what fraction of the decision is
// recency" rather than raw scale juggling. Defaults: recency-first,
// engagement second, graph third — a young network should surface
// what's happening NOW, not entrench incumbents.
type RankWeights struct {
	Recency    float64
	Graph      float64
	Engagement float64
}

// DefaultRankWeights returns the v1 weights.
func DefaultRankWeights() RankWeights {
	return RankWeights{Recency: 0.5, Graph: 0.2, Engagement: 0.3}
}

// Normalize scales the weights to sum 1. Non-positive or all-zero
// weights fall back to the defaults (fail-safe, never NaN).
func (w RankWeights) Normalize() RankWeights {
	total := w.Recency + w.Graph + w.Engagement
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return DefaultRankWeights()
	}
	return RankWeights{
		Recency:    w.Recency / total,
		Graph:      w.Graph / total,
		Engagement: w.Engagement / total,
	}
}

// RankSignals is one item's ranking inputs. Absent data (a post with no
// likes, an author with no followers) is the zero value — every term
// degrades gracefully to 0, never errors.
type RankSignals struct {
	// AuthorFollowers feeds the follow-graph boost.
	AuthorFollowers int64
	// Likes signals feed the engagement-velocity term.
	Likes engagement.PostSignals
}

// RankPage reorders page.Items in place by v1 RankScore, batch-loading
// the page's engagement signals: exactly two queries per page
// (`engagement.Signals` for like counts, `engagement.AuthorFollowerCounts`
// for follower counts), never per-item. It mutates the page in place —
// the caller owns the page by then — so the translate decoration step
// that follows sees the ranked order.
//
// eng == nil or a page with fewer than 2 items is a no-op. A
// signal-load error is returned to the caller; ranking must never turn
// a working feed into a 5xx, so callers log the error and serve the
// pre-rank (time, id) order.
func RankPage(ctx context.Context, eng *engagement.Service, page *FeedPage, w RankWeights) error {
	if eng == nil || page == nil || len(page.Items) < 2 {
		return nil
	}
	postIDs := make([]pgtype.UUID, 0, len(page.Items))
	seen := make(map[string]bool, len(page.Items))
	authorIDs := make([]pgtype.UUID, 0, len(page.Items))
	for _, it := range page.Items {
		var pid pgtype.UUID
		if err := pid.Scan(it.ID); err != nil || !pid.Valid {
			return fmt.Errorf("feed: rank: bad post id %q", it.ID)
		}
		postIDs = append(postIDs, pid)
		if !seen[it.AuthorID] {
			seen[it.AuthorID] = true
			var aid pgtype.UUID
			if err := aid.Scan(it.AuthorID); err != nil || !aid.Valid {
				return fmt.Errorf("feed: rank: bad author id %q", it.AuthorID)
			}
			authorIDs = append(authorIDs, aid)
		}
	}
	sigs, err := eng.Signals(ctx, postIDs)
	if err != nil {
		return fmt.Errorf("feed: rank: load like signals: %w", err)
	}
	counts, err := eng.AuthorFollowerCounts(ctx, authorIDs)
	if err != nil {
		return fmt.Errorf("feed: rank: load follower counts: %w", err)
	}
	signals := make(map[string]RankSignals, len(page.Items))
	for _, it := range page.Items {
		signals[it.ID] = RankSignals{
			AuthorFollowers: counts[it.AuthorID], // absent = zero
			Likes:           sigs[it.ID],         // absent = zero
		}
	}
	page.Items = RankItems(page.Items, signals, time.Now(), w)
	return nil
}

// RankScore computes the v1 score in [0,1]: weighted sum of three
// normalized terms. Deterministic: no I/O, no clock, no randomness.
func RankScore(createdAt, now time.Time, s RankSignals, w RankWeights) float64 {
	w = w.Normalize()

	ageHours := now.Sub(createdAt).Hours()
	if ageHours < 0 {
		ageHours = 0 // future-dated posts (clock skew) rank as fresh
	}

	// Recency: exponential decay, 2^(-age/halflife).
	recency := math.Exp2(-ageHours / RecencyHalfLifeHours)

	// Follow-graph boost: log10(1+followers) normalized, capped at 1.
	// Log scale: the jump 0→100 followers matters far more than
	// 100k→200k, which is the anti-incumbent property we want.
	followers := s.AuthorFollowers
	if followers < 0 {
		followers = 0
	}
	graph := math.Log10(1+float64(followers)) / math.Log10(1+GraphRefFollowers)
	graph = math.Min(graph, 1)

	// Engagement velocity: recent likes per hour, capped at the
	// reference rate. LikesLast24h is the trailing-24h window from
	// engagement.Signals; the denominator clamps to [1h, 24h] so a
	// minutes-old post with 2 likes doesn't score infinite velocity
	// and a days-old post isn't punished for its age twice.
	denom := math.Min(math.Max(ageHours, 1), VelocityWindowHours)
	velocity := float64(s.Likes.LikesLast24h) / denom
	engagement := math.Min(velocity/EngagementRefPerHour, 1)

	return w.Recency*recency + w.Graph*graph + w.Engagement*engagement
}

// RankItems reorders one feed page by RankScore, descending. Exact
// score ties keep the incoming (time, id) order — the tie-break is
// part of the sort, so ties are deterministic and reproduce the
// pre-ranking order. The input slice is not mutated; a ranked copy is
// returned (callers may reuse the original for cursor math).
func RankItems(items []FeedItem, signals map[string]RankSignals, now time.Time, w RankWeights) []FeedItem {
	type scored struct {
		item  FeedItem
		score float64
	}
	sc := make([]scored, len(items))
	for i, it := range items {
		sc[i] = scored{item: it, score: RankScore(it.CreatedAt, now, signals[it.ID], w)}
	}
	sort.SliceStable(sc, func(i, j int) bool {
		if sc[i].score != sc[j].score {
			return sc[i].score > sc[j].score
		}
		// Exact tie: fall back to the pre-rank order (time, id).
		a, b := sc[i].item, sc[j].item
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	out := make([]FeedItem, len(items))
	for i, s := range sc {
		out[i] = s.item
	}
	return out
}
