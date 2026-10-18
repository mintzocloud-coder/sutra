// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/engagement"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
)

// RankPage no-op matrix: nil eng, nil page, and pages that cannot be
// reordered must succeed without touching the DB.
func TestRankPageNoops(t *testing.T) {
	ctx := context.Background()
	eng := (*engagement.Service)(nil)
	if err := RankPage(ctx, eng, nil, DefaultRankWeights()); err != nil {
		t.Fatalf("nil page: %v", err)
	}
	if err := RankPage(ctx, eng, &FeedPage{}, DefaultRankWeights()); err != nil {
		t.Fatalf("empty page: %v", err)
	}
	solo := &FeedPage{Items: []FeedItem{{Post: posts.Post{ID: "x"}}}}
	if err := RankPage(ctx, eng, solo, DefaultRankWeights()); err != nil {
		t.Fatalf("single item: %v", err)
	}
	if solo.Items[0].ID != "x" {
		t.Fatal("single-item page must be untouched")
	}
}

// RankPage orders two same-author posts newest-first with no engagement,
// then a single like on the older post moves it above the newer one.
// The recency gap between two posts created milliseconds apart is
// ~1e-6 score points while the like contributes 0.3*0.1 = 0.03, so the
// flip is deterministic, not timing-luck.
func TestRankPageEngagementMovesPost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	authorID, _ := feedUser(t, pool, "Rankpage Author")
	likerID, _ := feedUser(t, pool, "Rankpage Liker")

	postSvc := posts.NewService(pool, 100000) // velocity cap irrelevant
	pOld, err := postSvc.Create(ctx, authorID, "rank candidate older", nil)
	if err != nil {
		t.Fatalf("create older: %v", err)
	}
	pNew, err := postSvc.Create(ctx, authorID, "rank candidate newer", nil)
	if err != nil {
		t.Fatalf("create newer: %v", err)
	}

	// Page as HomeFeed emits it: newest first.
	page := &FeedPage{Items: []FeedItem{{Post: *pNew}, {Post: *pOld}}}
	eng := engagement.NewService(pool)
	if err := RankPage(ctx, eng, page, DefaultRankWeights()); err != nil {
		t.Fatalf("rank baseline: %v", err)
	}
	if page.Items[0].ID != pNew.ID || page.Items[1].ID != pOld.ID {
		t.Fatalf("baseline order changed without engagement: %s, %s",
			page.Items[0].ID, page.Items[1].ID)
	}

	var oldID pgtype.UUID
	if err := oldID.Scan(pOld.ID); err != nil {
		t.Fatalf("scan post id: %v", err)
	}
	if _, _, err := eng.Like(ctx, oldID, likerID); err != nil {
		t.Fatalf("like: %v", err)
	}

	page = &FeedPage{Items: []FeedItem{{Post: *pNew}, {Post: *pOld}}}
	if err := RankPage(ctx, eng, page, DefaultRankWeights()); err != nil {
		t.Fatalf("rank after like: %v", err)
	}
	if page.Items[0].ID != pOld.ID || page.Items[1].ID != pNew.ID {
		t.Fatalf("like did not move older post up: %s, %s",
			page.Items[0].ID, page.Items[1].ID)
	}
}
