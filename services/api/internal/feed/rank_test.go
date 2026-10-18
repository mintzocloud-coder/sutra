// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"math"
	"testing"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/engagement"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
)

// Fixed clock for every test: the scorer takes `now` as a parameter,
// so nothing here depends on the wall clock.
var rankNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func TestRankScoreDeterministic(t *testing.T) {
	created := rankNow.Add(-3 * time.Hour)
	sig := RankSignals{AuthorFollowers: 1234, Likes: engagement.PostSignals{LikesTotal: 50, LikesLast24h: 7}}
	w := DefaultRankWeights()
	first := RankScore(created, rankNow, sig, w)
	for i := 0; i < 100; i++ {
		if got := RankScore(created, rankNow, sig, w); got != first {
			t.Fatalf("non-deterministic score: %v != %v on iteration %d", got, first, i)
		}
	}
}

func TestRankScoreHandComputed(t *testing.T) {
	w := DefaultRankWeights() // 0.5 / 0.2 / 0.3

	// Fresh post, zero signals: recency=1, graph=0, engagement=0.
	if got := RankScore(rankNow, rankNow, RankSignals{}, w); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("fresh zero-signal post: got %v, want 0.5", got)
	}
	// 12h old: recency halves exactly (half-life = 12h).
	if got := RankScore(rankNow.Add(-12*time.Hour), rankNow, RankSignals{}, w); math.Abs(got-0.25) > 1e-12 {
		t.Fatalf("12h-old post: got %v, want 0.25", got)
	}
	// 24h old: recency quarters.
	if got := RankScore(rankNow.Add(-24*time.Hour), rankNow, RankSignals{}, w); math.Abs(got-0.125) > 1e-12 {
		t.Fatalf("24h-old post: got %v, want 0.125", got)
	}
	// Reference followers: log10(1+100000)/log10(1+100000) = 1 exactly.
	sig := RankSignals{AuthorFollowers: GraphRefFollowers}
	want := 0.5*1 + 0.2*1 // recency 1 (fresh) + graph 1
	if got := RankScore(rankNow, rankNow, sig, w); math.Abs(got-want) > 1e-9 {
		t.Fatalf("ref-follower post: got %v, want %v", got, want)
	}
	// 100 followers: log10(101)/log10(100001) ≈ 0.4008 — small
	// creators are boosted, never zeroed.
	sig = RankSignals{AuthorFollowers: 100}
	graph := math.Log10(101) / math.Log10(1+GraphRefFollowers)
	want = 0.5 + 0.2*graph
	if got := RankScore(rankNow, rankNow, sig, w); math.Abs(got-want) > 1e-9 {
		t.Fatalf("100-follower post: got %v, want %v", got, want)
	}
	// Velocity cap: 240 likes in 24h on a 24h-old post = 10/h =
	// reference rate → engagement term exactly 1.
	sig = RankSignals{Likes: engagement.PostSignals{LikesTotal: 300, LikesLast24h: 240}}
	want = 0.5*0.25 + 0.3*1 // recency 2^-2, engagement 1
	if got := RankScore(rankNow.Add(-24*time.Hour), rankNow, sig, w); math.Abs(got-want) > 1e-9 {
		t.Fatalf("hot post: got %v, want %v", got, want)
	}
	// Denominator floor: 2 likes on a 5-minute-old post = 2/1h, not
	// 2/(5min) — no infinite velocity for minutes-old posts.
	sig = RankSignals{Likes: engagement.PostSignals{LikesTotal: 2, LikesLast24h: 2}}
	want = 0.5*math.Exp2(-(5.0/60)/12) + 0.3*(2.0/10)
	if got := RankScore(rankNow.Add(-5*time.Minute), rankNow, sig, w); math.Abs(got-want) > 1e-9 {
		t.Fatalf("young post velocity: got %v, want %v", got, want)
	}
	// Future-dated post (clock skew) ranks as fresh, never NaN.
	if got := RankScore(rankNow.Add(time.Hour), rankNow, RankSignals{}, w); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("future post: got %v, want 0.5", got)
	}
}

func TestRankScoreMonotonicity(t *testing.T) {
	w := DefaultRankWeights()
	base := RankSignals{AuthorFollowers: 500, Likes: engagement.PostSignals{LikesTotal: 10, LikesLast24h: 3}}

	newer := RankScore(rankNow.Add(-time.Hour), rankNow, base, w)
	older := RankScore(rankNow.Add(-48*time.Hour), rankNow, base, w)
	if !(newer > older) {
		t.Fatalf("recency not monotonic: newer=%v older=%v", newer, older)
	}

	hot := base
	hot.Likes.LikesLast24h = 30
	cold := base
	if !(RankScore(rankNow.Add(-6*time.Hour), rankNow, hot, w) >
		RankScore(rankNow.Add(-6*time.Hour), rankNow, cold, w)) {
		t.Fatalf("engagement not monotonic")
	}

	big := base
	big.AuthorFollowers = 50000
	small := base
	if !(RankScore(rankNow.Add(-6*time.Hour), rankNow, big, w) >
		RankScore(rankNow.Add(-6*time.Hour), rankNow, small, w)) {
		t.Fatalf("graph not monotonic")
	}
}

func TestRankScoreBounds(t *testing.T) {
	w := DefaultRankWeights()
	for _, age := range []time.Duration{0, time.Hour, 72 * time.Hour, 720 * time.Hour} {
		for _, followers := range []int64{0, 1, 100000, 50_000_000} {
			for _, likes := range []int64{0, 1, 1000, 1_000_000} {
				s := RankScore(rankNow.Add(-age), rankNow,
					RankSignals{AuthorFollowers: followers,
						Likes: engagement.PostSignals{LikesTotal: likes, LikesLast24h: likes}},
					w)
				if math.IsNaN(s) || s < 0 || s > 1 {
					t.Fatalf("score out of [0,1]: age=%v followers=%d likes=%d → %v",
						age, followers, likes, s)
				}
			}
		}
	}
}

func TestRankWeightsNormalize(t *testing.T) {
	w := RankWeights{Recency: 2, Graph: 1, Engagement: 1}.Normalize()
	if math.Abs(w.Recency+w.Graph+w.Engagement-1) > 1e-12 {
		t.Fatalf("weights do not sum to 1: %+v", w)
	}
	if math.Abs(w.Recency-0.5) > 1e-12 {
		t.Fatalf("normalization wrong: %+v", w)
	}
	// Degenerate weights fall back to defaults, never NaN.
	for _, bad := range []RankWeights{{}, {-1, -2, -3}, {math.NaN(), 1, 1}} {
		if got := bad.Normalize(); got != DefaultRankWeights() {
			t.Fatalf("bad weights %+v did not fall back to defaults: %+v", bad, got)
		}
	}
}

func mkItem(id string, age time.Duration) FeedItem {
	return FeedItem{Post: posts.Post{
		ID:        id,
		AuthorID:  "author",
		Text:      "x",
		CreatedAt: rankNow.Add(-age),
	}}
}

func TestRankItemsReordersByScore(t *testing.T) {
	w := DefaultRankWeights()
	items := []FeedItem{
		mkItem("old-quiet", 24*time.Hour), // recency 0.25 → ~0.125
		mkItem("new-quiet", time.Hour),    // recency ~0.944 → ~0.472
		mkItem("mid-hot", 2*time.Hour),    // velocity-capped → ~0.746
	}
	signals := map[string]RankSignals{
		"mid-hot": {Likes: engagement.PostSignals{LikesTotal: 60, LikesLast24h: 40}},
	}
	ranked := RankItems(items, signals, rankNow, w)
	want := []string{"mid-hot", "new-quiet", "old-quiet"}
	for i, id := range want {
		if ranked[i].ID != id {
			t.Fatalf("position %d: got %s, want %s (full order: %v)",
				i, ranked[i].ID, id, []string{ranked[0].ID, ranked[1].ID, ranked[2].ID})
		}
	}
}

func TestRankItemsTiesKeepInputOrder(t *testing.T) {
	w := DefaultRankWeights()
	// Identical timestamps + zero signals = identical scores; the
	// (time, id) tie-break is deterministic, not input-order
	// dependent: id desc wins.
	a := mkItem("post-a", time.Hour)
	b := mkItem("post-b", time.Hour)
	ranked := RankItems([]FeedItem{a, b}, nil, rankNow, w)
	if ranked[0].ID != "post-b" || ranked[1].ID != "post-a" {
		t.Fatalf("tie-break not deterministic: %s, %s", ranked[0].ID, ranked[1].ID)
	}
	// And the reverse input order converges to the same output.
	ranked2 := RankItems([]FeedItem{b, a}, nil, rankNow, w)
	if ranked2[0].ID != "post-b" || ranked2[1].ID != "post-a" {
		t.Fatalf("tie-break input-order dependent: %s, %s", ranked2[0].ID, ranked2[1].ID)
	}
}

func TestRankItemsDoesNotMutateInput(t *testing.T) {
	w := DefaultRankWeights()
	items := []FeedItem{mkItem("a", 24*time.Hour), mkItem("b", time.Hour)}
	signals := map[string]RankSignals{"b": {Likes: engagement.PostSignals{LikesLast24h: 100}}}
	_ = RankItems(items, signals, rankNow, w)
	if items[0].ID != "a" || items[1].ID != "b" {
		t.Fatalf("RankItems mutated its input: %v", []string{items[0].ID, items[1].ID})
	}
}
