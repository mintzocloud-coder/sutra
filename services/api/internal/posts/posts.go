// SPDX-License-Identifier: AGPL-3.0-or-later
// Package posts owns the post content store: creation, author timelines,
// deletion, and the per-author velocity cap.
//
// Design notes:
//   - posts is the source of truth. D3 part 2 layers Valkey sorted-set
//     home timelines (fan-out) on top; this package's reads stay the
//     pull path for high-follower authors and the author profile view.
//   - Text is capped at 500 runes (Telegram parity) — rune-counted so
//     emoji-heavy posts are measured fairly; the DB CHECK is the
//     backstop (char_length, same 500), Go returns 422s first.
//   - media_ids entries are shape-validated only. The "reference only
//     ready media" gate is enforced by the D4 media pipeline, not here.
//   - The velocity cap is a rolling 1-hour COUNT per author (default 30,
//     env-overridable). It is best-effort under concurrency: two racing
//     creates can both pass the check and both insert. That is the
//     standard trade for an abuse guard (a hard invariant would need a
//     serializing lock on every post, killing write throughput); the
//     race window is one query wide and the overshoot is bounded by the
//     number of concurrent writers, which the global rate limiter
//     already throttles.
//   - All listings are keyset-paginated on (created_at, id) — never
//     OFFSET — so pages stay stable while authors keep posting.
package posts

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// Content limits. Text is measured in runes (see package doc); media
// ids are opaque handles minted by the media service (D4).
const (
	MaxPostLenRunes    = 500 // post body cap
	MaxMediaIDsPerPost = 10  // sane product cap (also a DB CHECK)
	MaxMediaIDLen      = 128 // bytes
	DefaultVelocityCap = 30  // posts per rolling hour per author
	MaxPageLimit       = 100
	DefaultPageLimit   = 20
)

// mediaIDRe is the media-id grammar: the media service (D4) mints ids
// in this shape; anything else is rejected as 422, never stored.
var mediaIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var (
	// ErrPostNotFound is returned when the post id does not exist.
	ErrPostNotFound = errors.New("posts: post not found")
	// ErrNotAuthor is returned when a non-author tries to delete.
	ErrNotAuthor = errors.New("posts: only the author can delete this post")
	// ErrEmptyText is returned when the post body is blank.
	ErrEmptyText = errors.New("posts: post text must not be blank")
	// ErrTextTooLong is returned when the body exceeds 500 runes.
	ErrTextTooLong = errors.New("posts: post text exceeds 500 characters")
	// ErrTooManyMedia is returned when more than 10 media ids attach.
	ErrTooManyMedia = errors.New("posts: too many media attachments (max 10)")
	// ErrBadMediaID is returned when a media id breaks the grammar.
	ErrBadMediaID = errors.New("posts: media id must match [A-Za-z0-9_-]{1,128}")
)

// VelocityLimitedError is returned when the author exceeds their rolling
// posts-per-hour cap. RetryAfter tells the client (and the 429
// Retry-After header) when the oldest post in the window ages out.
type VelocityLimitedError struct {
	RetryAfter time.Duration
	Cap        int
}

func (e *VelocityLimitedError) Error() string {
	return "posts: post velocity cap exceeded"
}

// Post is the API representation of one post.
type Post struct {
	ID        string    `json:"id"`
	AuthorID  string    `json:"author_id"`
	Text      string    `json:"text"`
	MediaIDs  []string  `json:"media_ids"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PostPage is one keyset-paginated author-timeline page, newest first.
type PostPage struct {
	Items      []Post  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// Service is the post store. velocityCapPerHour is the rolling 1-hour
// per-author cap; <=0 selects DefaultVelocityCap.
type Service struct {
	pool               *db.Pool
	velocityCapPerHour int
}

// NewService builds the post store.
func NewService(pool *db.Pool, velocityCapPerHour int) *Service {
	if velocityCapPerHour <= 0 {
		velocityCapPerHour = DefaultVelocityCap
	}
	return &Service{pool: pool, velocityCapPerHour: velocityCapPerHour}
}

// ValidateMediaIDs checks the attachment list: count cap then per-id
// grammar. Shape only — readiness is the D4 gate.
func ValidateMediaIDs(mediaIDs []string) error {
	if len(mediaIDs) > MaxMediaIDsPerPost {
		return ErrTooManyMedia
	}
	for _, id := range mediaIDs {
		if !mediaIDRe.MatchString(id) {
			return ErrBadMediaID
		}
	}
	return nil
}

// validateText checks the body: non-blank, within the rune cap.
func validateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return ErrEmptyText
	}
	if utf8.RuneCountInString(text) > MaxPostLenRunes {
		return ErrTextTooLong
	}
	return nil
}

// checkVelocity counts the author's posts in the trailing hour. When the
// cap is hit it returns *VelocityLimitedError carrying the wait until
// the oldest post in the window ages out (rounded up, minimum 1s).
func (s *Service) checkVelocity(ctx context.Context, authorID pgtype.UUID) error {
	var (
		count  int64
		oldest pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at)
		FROM posts
		WHERE author_id = $1
		  AND created_at > now() - interval '1 hour'`,
		authorID).Scan(&count, &oldest)
	if err != nil {
		return err
	}
	if count < int64(s.velocityCapPerHour) {
		return nil
	}
	retryAfter := time.Second
	if oldest.Valid {
		wait := oldest.Time.Add(time.Hour).Sub(time.Now().UTC())
		if wait > retryAfter {
			// Round up so the client never retries a second early.
			retryAfter = wait.Truncate(time.Second) + time.Second
		}
	}
	return &VelocityLimitedError{RetryAfter: retryAfter, Cap: s.velocityCapPerHour}
}

// Create stores a new post for authorID after validation and the
// velocity check. mediaIDs may be nil (stored as the empty list).
func (s *Service) Create(ctx context.Context, authorID pgtype.UUID, text string, mediaIDs []string) (*Post, error) {
	if err := validateText(text); err != nil {
		return nil, err
	}
	if err := ValidateMediaIDs(mediaIDs); err != nil {
		return nil, err
	}
	if err := s.checkVelocity(ctx, authorID); err != nil {
		return nil, err
	}
	if mediaIDs == nil {
		mediaIDs = []string{}
	}

	var p Post
	var id pgtype.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO posts (author_id, text, media_ids)
		VALUES ($1, $2, $3)
		RETURNING id, author_id, text, media_ids, created_at, updated_at`,
		authorID, text, mediaIDs).
		Scan(&id, &p.AuthorID, &p.Text, &p.MediaIDs, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.ID = id.String()
	p.AuthorID = authorID.String()
	if p.MediaIDs == nil {
		p.MediaIDs = []string{}
	}
	return &p, nil
}

// scanPost maps one post row into a Post.
func scanPost(row pgx.Row) (*Post, error) {
	var (
		p        Post
		id       pgtype.UUID
		author   pgtype.UUID
		mediaIDs []string
	)
	if err := row.Scan(&id, &author, &p.Text, &mediaIDs, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.ID = id.String()
	p.AuthorID = author.String()
	p.MediaIDs = mediaIDs
	if p.MediaIDs == nil {
		p.MediaIDs = []string{}
	}
	return &p, nil
}

// Get returns one post by id. Unknown ids are ErrPostNotFound.
func (s *Service) Get(ctx context.Context, id pgtype.UUID) (*Post, error) {
	p, err := scanPost(s.pool.QueryRow(ctx, `
		SELECT id, author_id, text, media_ids, created_at, updated_at
		FROM posts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPostNotFound
	}
	return p, err
}

// ListByAuthor pages one author's posts, newest first, keyset on
// (created_at, id). The caller checks the author exists (404); the
// service treats an unknown author as an empty listing.
func (s *Service) ListByAuthor(ctx context.Context, authorID pgtype.UUID, limit int, after *social.Cursor) (*PostPage, error) {
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}

	keyset := ""
	args := []any{authorID}
	if after != nil {
		var postID pgtype.UUID
		if err := postID.Scan(after.ID); err != nil || !postID.Valid {
			return nil, social.ErrInvalidCursor
		}
		args = append(args, after.T, postID)
		keyset = "AND (created_at, id) < ($2, $3)"
	}
	args = append(args, limit+1) // fetch one extra to detect a next page

	rows, err := s.pool.Query(ctx, `
		SELECT id, author_id, text, media_ids, created_at, updated_at
		FROM posts
		WHERE author_id = $1 `+keyset+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &PostPage{Items: []Post{}}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(page.Items) > limit {
		// Cursor from the last RETURNED item (not the lookahead row):
		// the next page starts strictly after it.
		last := page.Items[limit-1]
		page.Items = page.Items[:limit]
		c := social.EncodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &c
	}
	return page, nil
}

// Delete removes a post. Unknown ids are ErrPostNotFound; a caller who
// is not the author gets ErrNotAuthor. Idempotency is NOT offered here
// (unlike follow/unfollow): deleting an already-gone post is a client
// bug worth surfacing as 404, and the D3 acceptance criteria call for
// author-only deletion.
func (s *Service) Delete(ctx context.Context, id, callerID pgtype.UUID) error {
	var authorID pgtype.UUID
	err := s.pool.QueryRow(ctx, `SELECT author_id FROM posts WHERE id = $1`, id).Scan(&authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPostNotFound
	}
	if err != nil {
		return err
	}
	if authorID != callerID {
		return ErrNotAuthor
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM posts WHERE id = $1`, id)
	return err
}
