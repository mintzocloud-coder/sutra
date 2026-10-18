// SPDX-License-Identifier: AGPL-3.0-or-later
// Package engagement owns the like primitive: the only engagement
// signal in Wave 2 v1 and the raw material for the "engagement
// velocity" term of the home-feed ranker (see docs/recsys.md and
// internal/feed/rank.go).
//
// Design notes:
//   - Likes are idempotent by construction: the (post_id, user_id)
//     composite PK plus INSERT ... ON CONFLICT DO NOTHING means a
//     retried like is a no-op, never a double count — the same
//     at-least-once safety the chat service applies to message sends
//     (docs/delivery-semantics.md).
//   - Non-ML by design. Issue #19 (pgvector embeddings pipeline) is
//     reserved for Hacktoberfest contributors; nothing here builds
//     embeddings or ML features.
//   - Unlike is best-effort idempotent: unliking an unliked post is a
//     no-op success, like follows' DELETE /follow (D2).
//   - Signal loading is batched: Signals and AuthorFollowerCounts each
//     cost exactly one query per feed page, never per item.
package engagement

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// ErrPostNotFound is returned when a like targets a post that does not
// exist (or was deleted). Callers map it to 404.
var ErrPostNotFound = errors.New("engagement: post not found")

// PostSignals is one post's engagement snapshot for ranking.
type PostSignals struct {
	// LikesTotal is the all-time like count.
	LikesTotal int64
	// LikesLast24h counts likes in the trailing 24 hours — the
	// velocity window. A post with 100 old likes and 0 recent ones is
	// stale; a post with 5 likes all in the last hour is hot.
	LikesLast24h int64
}

// Service is the like store.
type Service struct {
	pool *db.Pool
}

// NewService builds the like store.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// postExists reports whether the post row is present.
func (s *Service) postExists(ctx context.Context, postID pgtype.UUID) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM posts WHERE id = $1)`, postID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// Like records userID's like of postID. Idempotent: liking twice
// returns created=false with the current count. Unknown posts are
// ErrPostNotFound, never silently liked.
func (s *Service) Like(ctx context.Context, postID, userID pgtype.UUID) (created bool, likeCount int64, err error) {
	ok, err := s.postExists(ctx, postID)
	if err != nil {
		return false, 0, err
	}
	if !ok {
		return false, 0, ErrPostNotFound
	}
	// RETURNING post_id only yields a row on a real insert; the
	// conflict path yields nothing. No separate SELECT, no race.
	var inserted pgtype.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO likes (post_id, user_id) VALUES ($1, $2)
		ON CONFLICT (post_id, user_id) DO NOTHING
		RETURNING post_id`, postID, userID).Scan(&inserted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, err
	}
	created = inserted.Valid
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM likes WHERE post_id = $1`, postID).Scan(&likeCount); err != nil {
		return false, 0, err
	}
	return created, likeCount, nil
}

// Unlike removes userID's like of postID. Idempotent: unliking an
// unliked (or unknown) post is a no-op success returning the current
// count. Unlike Like, unknown posts are NOT an error — DELETE
// semantics are "ensure absent".
func (s *Service) Unlike(ctx context.Context, postID, userID pgtype.UUID) (removed bool, likeCount int64, err error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM likes WHERE post_id = $1 AND user_id = $2`, postID, userID)
	if err != nil {
		return false, 0, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM likes WHERE post_id = $1`, postID).Scan(&likeCount); err != nil {
		return false, 0, err
	}
	return tag.RowsAffected() > 0, likeCount, nil
}

// Signals batch-loads engagement snapshots for postIDs: one GROUP BY
// query for the whole page. Posts with no likes are absent from the
// map; callers treat absence as zero.
func (s *Service) Signals(ctx context.Context, postIDs []pgtype.UUID) (map[string]PostSignals, error) {
	out := make(map[string]PostSignals, len(postIDs))
	if len(postIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT post_id,
		       count(*) AS total,
		       count(*) FILTER (WHERE created_at >= now() - interval '24 hours') AS last24h
		FROM likes
		WHERE post_id = ANY($1)
		GROUP BY post_id`, postIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  pgtype.UUID
			sig PostSignals
		)
		if err := rows.Scan(&id, &sig.LikesTotal, &sig.LikesLast24h); err != nil {
			return nil, err
		}
		out[id.String()] = sig
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AuthorFollowerCounts batch-loads follower counts for authorIDs: one
// GROUP BY query for the whole page. Authors with no followers are
// absent from the map; callers treat absence as zero.
func (s *Service) AuthorFollowerCounts(ctx context.Context, authorIDs []pgtype.UUID) (map[string]int64, error) {
	out := make(map[string]int64, len(authorIDs))
	if len(authorIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT followee_id, count(*)
		FROM follows
		WHERE followee_id = ANY($1)
		GROUP BY followee_id`, authorIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id pgtype.UUID
			n  int64
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id.String()] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
