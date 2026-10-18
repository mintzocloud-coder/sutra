// SPDX-License-Identifier: AGPL-3.0-or-later
// Package feed owns the home timeline: a hybrid fan-out / fan-in feed
// on top of the posts store (D3b).
//
// Design (see docs/feeds.md):
//   - Push path: when a post is created, it is fanned out into a Valkey
//     sorted set per follower (`feed:home:{userID}`), member = post id,
//     score = post created_at as unix microseconds. The author's own
//     post lands in their own timeline too. Timelines are trimmed to
//     timelineMax entries and carry a 30-day TTL.
//   - Pull path: authors with MORE than fanoutThreshold followers are
//     "celebrities" — their posts are NOT fanned out; the home feed
//     reads them from PostgreSQL at request time instead. This bounds
//     the write amplification of a viral author.
//   - GET /v1/feed/home merges the two streams with a k-way merge over
//     (time, id), dedupes by post id (a post can legitimately appear in
//     both streams when an author crosses the threshold), rehydrates
//     pushed ids from PostgreSQL (the source of truth — missing rows
//     are dropped), and pages with per-source cursors (never OFFSET).
//   - Delete propagation: every fan-out records its recipients in
//     `feed:postfans:{postID}`; deleting a post ZREMs it from exactly
//     those timelines. Celebrity posts need no propagation (nothing was
//     fanned out); the rehydration step drops missing posts anyway, so
//     a lost propagation can only leave a hole, never a ghost post.
//
// Failure semantics (documented, deliberate):
//   - Fan-out failure on create: the post is deleted again
//     (compensating rollback, done by the HTTP handler) and the request
//     fails 503 — a retry is safe because the first attempt never became
//     visible. 201 always means "visible in home timelines".
//   - Unfan-out failure on delete: best-effort; the read path drops
//     posts missing from PostgreSQL, so the feed self-heals.
//   - Valkey down on read: the feed fails 503 (ErrFeedUnavailable).
//     Author timelines and post CRUD keep working — they are pure
//     PostgreSQL.
package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// Tunables. Thresholds are env-configurable (see internal/config);
// these are the defaults.
const (
	// DefaultFanoutThreshold bounds write amplification: an author with
	// more followers than this is served pull-on-read instead of
	// fan-out-on-write.
	DefaultFanoutThreshold = 10000
	// DefaultTimelineMax caps the entries kept per home timeline.
	// Nobody scrolls deeper; the cap bounds Valkey memory per user.
	DefaultTimelineMax = 2000
	// homeTTL refreshes on every fan-out write so active users keep
	// their timeline while abandoned accounts age out of memory.
	homeTTL = 30 * 24 * time.Hour
	// MaxPageLimit / DefaultPageLimit mirror the posts package so the
	// feed pages like every other listing.
	MaxPageLimit     = 100
	DefaultPageLimit = 20
)

var (
	// ErrInvalidCursor is returned when a feed cursor is malformed.
	ErrInvalidCursor = errors.New("feed: invalid cursor")
	// ErrFeedUnavailable wraps Valkey failures on the read/write path.
	// Callers map it to 503, never 500: it is an infrastructure outage,
	// not a bug.
	ErrFeedUnavailable = errors.New("feed: timeline store unavailable")
)

// homeKey is the per-user timeline sorted set.
func homeKey(userID string) string { return "feed:home:" + userID }

// fansKey is the reverse index: the set of user ids a post was fanned
// out to, used for exact delete propagation.
func fansKey(postID string) string { return "feed:postfans:" + postID }

// scoreOf maps a post time to its sorted-set score: unix microseconds
// as float64. 1.8e12 fits exactly in a float64 mantissa (2^53 ≈ 9e15),
// so ordering is lossless — unlike unix NANOSECONDS, which would lose
// precision and corrupt the timeline order.
func scoreOf(t time.Time) float64 { return float64(t.UnixMicro()) }

// timeOf inverts scoreOf.
func timeOf(score float64) time.Time { return time.UnixMicro(int64(score)).UTC() }

// formatScore renders a score for exclusive/inclusive ZRANGEBYSCORE
// bounds. 'f'/-1 is the shortest round-trip representation, so the
// bound Redis parses is exactly the score we stored.
func formatScore(score float64) string {
	return strconv.FormatFloat(score, 'f', -1, 64)
}

// Service is the home-timeline store.
type Service struct {
	pool            *db.Pool
	rdb             *redis.Client
	fanoutThreshold int
	timelineMax     int
}

// NewService builds the feed service. Non-positive threshold/max
// select the defaults.
func NewService(pool *db.Pool, rdb *redis.Client, fanoutThreshold, timelineMax int) *Service {
	if fanoutThreshold <= 0 {
		fanoutThreshold = DefaultFanoutThreshold
	}
	if timelineMax <= 0 {
		timelineMax = DefaultTimelineMax
	}
	return &Service{pool: pool, rdb: rdb, fanoutThreshold: fanoutThreshold, timelineMax: timelineMax}
}

// FanoutPost fans a freshly created post out to the author's followers'
// home timelines (plus the author's own). Authors above the follower
// threshold are celebrities: nothing is written, the pull path serves
// them. A Valkey error is returned wrapped in ErrFeedUnavailable so
// the caller can compensate (see package doc).
func (s *Service) FanoutPost(ctx context.Context, post *posts.Post) error {
	var authorID pgtype.UUID
	if err := authorID.Scan(post.AuthorID); err != nil {
		return fmt.Errorf("feed: bad author id: %w", err)
	}

	var followerCount int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM follows WHERE followee_id = $1`,
		authorID).Scan(&followerCount); err != nil {
		return fmt.Errorf("feed: count followers: %w", err)
	}
	if followerCount > int64(s.fanoutThreshold) {
		return nil // celebrity: pull-on-read, no fan-out
	}

	rows, err := s.pool.Query(ctx,
		`SELECT follower_id FROM follows WHERE followee_id = $1`, authorID)
	if err != nil {
		return fmt.Errorf("feed: list followers: %w", err)
	}
	recipients := []string{post.AuthorID} // the author sees their own post
	for rows.Next() {
		var fid pgtype.UUID
		if err := rows.Scan(&fid); err != nil {
			rows.Close()
			return fmt.Errorf("feed: scan follower: %w", err)
		}
		recipients = append(recipients, fid.String())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("feed: list followers: %w", err)
	}

	score := scoreOf(post.CreatedAt)
	pipe := s.rdb.Pipeline()
	for _, uid := range recipients {
		key := homeKey(uid)
		pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: post.ID})
		// Trim oldest-first: keep the newest timelineMax entries.
		pipe.ZRemRangeByRank(ctx, key, 0, int64(-(s.timelineMax + 1)))
		pipe.Expire(ctx, key, homeTTL)
	}
	fans := fansKey(post.ID)
	members := make([]any, len(recipients))
	for i, uid := range recipients {
		members[i] = uid
	}
	pipe.SAdd(ctx, fans, members...)
	pipe.Expire(ctx, fans, homeTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("%w: fan-out pipeline: %v", ErrFeedUnavailable, err)
	}
	return nil
}

// UnfanoutPost removes a deleted post from every timeline it was fanned
// out to, using the reverse index. A missing index (celebrity post, or
// an expired set) is a no-op. Failures are returned wrapped in
// ErrFeedUnavailable; the caller treats them as best-effort because
// the read path drops posts missing from PostgreSQL anyway.
func (s *Service) UnfanoutPost(ctx context.Context, postID string) error {
	fans := fansKey(postID)
	members, err := s.rdb.SMembers(ctx, fans).Result()
	if err != nil {
		return fmt.Errorf("%w: read fan-out set: %v", ErrFeedUnavailable, err)
	}
	if len(members) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for _, uid := range members {
		pipe.ZRem(ctx, homeKey(uid), postID)
	}
	pipe.Del(ctx, fans)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("%w: unfan-out pipeline: %v", ErrFeedUnavailable, err)
	}
	return nil
}

// PushedCursor resumes the Valkey stream: entries strictly after
// (Score, Member) in (score DESC, member DESC) order.
type PushedCursor struct {
	Score  float64 `json:"s"`
	Member string  `json:"m"`
}

// FeedCursor is the opaque home-feed cursor: per-source resume points.
// Pushed == nil means "fetch the pushed stream from the newest";
// PushedDone skips the pushed stream (exhausted); same for Pull.
type FeedCursor struct {
	Pushed     *PushedCursor  `json:"p,omitempty"`
	PushedDone bool           `json:"pd,omitempty"`
	Pull       *social.Cursor `json:"c,omitempty"`
	PullDone   bool           `json:"cd,omitempty"`
}

// EncodeFeedCursor serializes a cursor for the next_cursor field. A nil
// cursor (end of feed) encodes as an empty string.
func EncodeFeedCursor(c *FeedCursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeFeedCursor parses a client-supplied cursor. Garbage in →
// ErrInvalidCursor, never a store error.
func DecodeFeedCursor(s string) (*FeedCursor, error) {
	var c FeedCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, ErrInvalidCursor
	}
	if c.Pushed != nil {
		if c.Pushed.Score <= 0 {
			return nil, ErrInvalidCursor
		}
		var m pgtype.UUID
		if err := m.Scan(c.Pushed.Member); err != nil || !m.Valid {
			return nil, ErrInvalidCursor
		}
	}
	if c.Pull != nil {
		if c.Pull.T.IsZero() || c.Pull.ID == "" {
			return nil, ErrInvalidCursor
		}
		var id pgtype.UUID
		if err := id.Scan(c.Pull.ID); err != nil || !id.Valid {
			return nil, ErrInvalidCursor
		}
	}
	return &c, nil
}

// PushedEntry is one timeline entry from Valkey.
type PushedEntry struct {
	ID    string
	Score float64
}

// pulledEntry is one row from the celebrity pull query.
type pulledEntry struct {
	post posts.Post
}

// FeedPage is one merged home-feed page, newest first.
type FeedPage struct {
	Items      []posts.Post `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

// fetchPushed reads the caller's Valkey timeline: limit+1 entries
// (the extra detects exhaustion), newest first. With a cursor it
// returns entries strictly after (Score, Member) in
// (score DESC, member DESC) order: the same-score members sorting after
// the cursor member first, then lower scores. Within a score, Valkey
// orders members lexicographically, so the tied tail is read ascending
// and emitted in reverse.
func (s *Service) fetchPushed(ctx context.Context, userID string, limit int, after *PushedCursor) ([]PushedEntry, bool, error) {
	key := homeKey(userID)
	want := int64(limit + 1)

	if after == nil {
		zs, err := s.rdb.ZRevRangeWithScores(ctx, key, 0, want-1).Result()
		if err != nil {
			return nil, true, fmt.Errorf("%w: read timeline: %v", ErrFeedUnavailable, err)
		}
		return zsetEntries(zs), int64(len(zs)) < want, nil
	}

	// Same-score tail: every member at exactly the cursor score sorts
	// before lower scores, so it leads the page. The query is uncapped —
	// same-microsecond ties are tiny in practice, and completeness here
	// is what keeps the resume cursor from skipping them.
	tied, err := s.rdb.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min: formatScore(after.Score),
		Max: formatScore(after.Score),
	}).Result()
	if err != nil {
		return nil, true, fmt.Errorf("%w: read timeline: %v", ErrFeedUnavailable, err)
	}
	out := make([]PushedEntry, 0, want)
	for i := len(tied) - 1; i >= 0; i-- {
		z := tied[i]
		member, _ := z.Member.(string)
		if member < after.Member {
			out = append(out, PushedEntry{ID: member, Score: z.Score})
		}
	}

	// Scores strictly below the cursor, newest first, capped. A short
	// read (< want) proves nothing more exists below the cursor.
	primary, err := s.rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    "(" + formatScore(after.Score), // exclusive
		Offset: 0,
		Count:  want,
	}).Result()
	if err != nil {
		return nil, true, fmt.Errorf("%w: read timeline: %v", ErrFeedUnavailable, err)
	}
	primaryEntries := zsetEntries(primary)
	primaryExhausted := int64(len(primaryEntries)) < want
	out = append(out, primaryEntries...)

	// Exhausted only when the page holds everything that exists after
	// the cursor: the tied tail is always complete, so that is exactly
	// when the primary read came back short. A truncated page always
	// has more behind it.
	exhausted := primaryExhausted && int64(len(out)) <= want
	if int64(len(out)) > want {
		out = out[:want]
		exhausted = false
	}
	return out, exhausted, nil
}

// zsetEntries maps go-redis Z values to PushedEntries. Members are
// always strings (we only ever ZADD string post ids).
func zsetEntries(zs []redis.Z) []PushedEntry {
	out := make([]PushedEntry, 0, len(zs))
	for _, z := range zs {
		member, _ := z.Member.(string)
		out = append(out, PushedEntry{ID: member, Score: z.Score})
	}
	return out
}

// fetchPulled reads posts by followed celebrity authors (follower count
// above the threshold) from PostgreSQL, newest first, keyset-paginated
// on (created_at, id). Returns limit+1 rows; the caller trims.
func (s *Service) fetchPulled(ctx context.Context, userID pgtype.UUID, limit int, after *social.Cursor) ([]posts.Post, bool, error) {
	keyset := ""
	args := []any{userID, s.fanoutThreshold}
	if after != nil {
		var id pgtype.UUID
		if err := id.Scan(after.ID); err != nil || !id.Valid {
			return nil, true, ErrInvalidCursor
		}
		args = append(args, after.T, id)
		keyset = "AND (p.created_at, p.id) < ($3, $4)"
	}
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.author_id, p.text, p.media_ids, p.created_at, p.updated_at
		FROM posts p
		WHERE p.author_id IN (
			SELECT f.followee_id
			FROM follows f
			WHERE f.follower_id = $1
			  AND (SELECT count(*) FROM follows f2
			       WHERE f2.followee_id = f.followee_id) > $2
		) `+keyset+`
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, true, fmt.Errorf("feed: pull celebrity posts: %w", err)
	}
	defer rows.Close()

	var out []posts.Post
	for rows.Next() {
		var (
			p        posts.Post
			id       pgtype.UUID
			author   pgtype.UUID
			mediaIDs []string
		)
		if err := rows.Scan(&id, &author, &p.Text, &mediaIDs, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, true, fmt.Errorf("feed: scan pulled post: %w", err)
		}
		p.ID = id.String()
		p.AuthorID = author.String()
		p.MediaIDs = mediaIDs
		if p.MediaIDs == nil {
			p.MediaIDs = []string{}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, true, fmt.Errorf("feed: pull celebrity posts: %w", err)
	}
	return out, len(out) < limit+1, nil
}

// mergeEntry is one candidate for the merged page.
type mergeEntry struct {
	id     string
	t      time.Time
	pushed bool // which stream it came from (for cursor math)
}

// mergeEntries k-way merges the pushed and pulled streams (both newest
// first) into the top-`limit` merged entries, deduping by post id. It
// returns the merged entries plus how many entries were CONSUMED from
// each stream (stream positions advanced past, including deduped
// entries — the cursor math needs positions, not returned posts).
func mergeEntries(pushed []PushedEntry, pulled []posts.Post, limit int) (merged []mergeEntry, consumedPushed, consumedPulled int) {
	seen := make(map[string]bool)
	i, j := 0, 0
	for len(merged) < limit && (i < len(pushed) || j < len(pulled)) {
		var takePushed bool
		switch {
		case i >= len(pushed):
			takePushed = false
		case j >= len(pulled):
			takePushed = true
		default:
			pt, qt := timeOf(pushed[i].Score), pulled[j].CreatedAt
			// Newest first: larger time wins; ties break on id —
			// ids are globally unique so the order is total.
			takePushed = pt.After(qt) || (pt.Equal(qt) && pushed[i].ID > pulled[j].ID)
		}
		var e mergeEntry
		if takePushed {
			e = mergeEntry{id: pushed[i].ID, t: timeOf(pushed[i].Score), pushed: true}
			i++
			consumedPushed++
		} else {
			e = mergeEntry{id: pulled[j].ID, t: pulled[j].CreatedAt, pushed: false}
			j++
			consumedPulled++
		}
		if seen[e.id] {
			continue // same post in both streams (threshold crossing)
		}
		seen[e.id] = true
		merged = append(merged, e)
	}
	return merged, consumedPushed, consumedPulled
}

// HomeFeed returns one merged home-feed page for userID, newest first.
// A nil cursor starts from the newest; the returned page carries the
// cursor for the next page (nil when the feed is exhausted).
func (s *Service) HomeFeed(ctx context.Context, userID pgtype.UUID, limit int, cursor *FeedCursor) (*FeedPage, error) {
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}

	// Pushed stream (Valkey).
	var pushed []PushedEntry
	pushedExhausted := true
	if cursor == nil || !cursor.PushedDone {
		var after *PushedCursor
		if cursor != nil {
			after = cursor.Pushed
		}
		entries, exhausted, err := s.fetchPushed(ctx, userID.String(), limit, after)
		if err != nil {
			return nil, err
		}
		pushed, pushedExhausted = entries, exhausted
	}

	// Pull stream (PostgreSQL celebrities).
	var pulled []posts.Post
	pullExhausted := true
	if cursor == nil || !cursor.PullDone {
		var after *social.Cursor
		if cursor != nil {
			after = cursor.Pull
		}
		list, exhausted, err := s.fetchPulled(ctx, userID, limit, after)
		if err != nil {
			return nil, err
		}
		pulled, pullExhausted = list, exhausted
	}

	merged, consumedPushed, consumedPulled := mergeEntries(pushed, pulled, limit)

	// Rehydrate: pulled posts are already in hand; pushed ids resolve
	// against PostgreSQL (source of truth). Missing rows (deleted
	// between fan-out and read) are dropped — a hole, never a ghost.
	byID := make(map[string]*posts.Post, len(merged))
	for _, p := range pulled {
		cp := p
		byID[cp.ID] = &cp
	}
	var needIDs []pgtype.UUID
	needIdx := make(map[string]bool)
	for _, m := range merged {
		if m.pushed {
			if _, ok := byID[m.id]; !ok && !needIdx[m.id] {
				var id pgtype.UUID
				if err := id.Scan(m.id); err == nil && id.Valid {
					needIDs = append(needIDs, id)
					needIdx[m.id] = true
				}
			}
		}
	}
	if len(needIDs) > 0 {
		rehydrated, err := s.loadPosts(ctx, needIDs)
		if err != nil {
			return nil, err
		}
		for id, p := range rehydrated {
			byID[id] = p
		}
	}

	page := &FeedPage{Items: []posts.Post{}}
	for _, m := range merged {
		if p, ok := byID[m.id]; ok {
			page.Items = append(page.Items, *p)
		}
	}

	// Next cursor from per-source stream positions. A stream is marked
	// done only when the fetch reported exhaustion AND the merge consumed
	// every fetched entry. If the merge starved a stream — took all
	// `limit` entries from the other one — unconsumed entries remain, and
	// marking the stream done would silently DROP those posts. Instead the
	// stream resumes after its last consumed entry (or re-issues the
	// incoming cursor when nothing was consumed); advancing past
	// unconsumed rows would SKIP them.
	next := &FeedCursor{}
	switch {
	case consumedPushed < len(pushed) && consumedPushed > 0:
		last := pushed[consumedPushed-1]
		next.Pushed = &PushedCursor{Score: last.Score, Member: last.ID}
	case consumedPushed < len(pushed): // == 0: starved; re-issue cursor
		if cursor != nil {
			next.Pushed = cursor.Pushed // may be nil = fetch from newest
		}
	case pushedExhausted:
		next.PushedDone = true
	case consumedPushed > 0:
		last := pushed[consumedPushed-1]
		next.Pushed = &PushedCursor{Score: last.Score, Member: last.ID}
	default: // empty page, not exhausted: cannot happen (a non-exhausted
		// fetch always returns entries), but re-issue rather than panic.
		if cursor != nil {
			next.Pushed = cursor.Pushed
		}
	}
	switch {
	case consumedPulled < len(pulled) && consumedPulled > 0:
		last := pulled[consumedPulled-1]
		next.Pull = &social.Cursor{T: last.CreatedAt, ID: last.ID}
	case consumedPulled < len(pulled): // == 0: starved; re-issue cursor
		if cursor != nil {
			next.Pull = cursor.Pull
		}
	case pullExhausted:
		next.PullDone = true
	case consumedPulled > 0:
		last := pulled[consumedPulled-1]
		next.Pull = &social.Cursor{T: last.CreatedAt, ID: last.ID}
	default: // empty page, not exhausted: re-issue rather than panic.
		if cursor != nil {
			next.Pull = cursor.Pull
		}
	}
	if next.PushedDone && next.PullDone {
		return page, nil // end of feed: no cursor
	}
	c := EncodeFeedCursor(next)
	page.NextCursor = &c
	return page, nil
}

// loadPosts batch-loads posts by id, keyed by id string. Missing ids
// are absent from the map.
func (s *Service) loadPosts(ctx context.Context, ids []pgtype.UUID) (map[string]*posts.Post, error) {
	out := make(map[string]*posts.Post, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, author_id, text, media_ids, created_at, updated_at
		FROM posts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("feed: rehydrate posts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p        posts.Post
			id       pgtype.UUID
			author   pgtype.UUID
			mediaIDs []string
		)
		if err := rows.Scan(&id, &author, &p.Text, &mediaIDs, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("feed: scan rehydrated post: %w", err)
		}
		p.ID = id.String()
		p.AuthorID = author.String()
		p.MediaIDs = mediaIDs
		if p.MediaIDs == nil {
			p.MediaIDs = []string{}
		}
		out[p.ID] = &p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feed: rehydrate posts: %w", err)
	}
	return out, nil
}
