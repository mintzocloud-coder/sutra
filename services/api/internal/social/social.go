// SPDX-License-Identifier: AGPL-3.0-or-later
// Package social owns the public social graph: profiles and follows.
//
// Design notes:
//   - users rows are the identity record (OIDC subject); profiles rows
//     are the PUBLIC identity, created lazily on the first PATCH — a user
//     with no profile row still reads fine (handle NULL, display name
//     falls back to the OIDC-seeded users.display_name).
//   - Handle claims are first-come: the UNIQUE constraint on
//     profiles.handle is the single serialization point, so concurrent
//     claim races converge on exactly one winner; the loser gets a 409.
//   - follows edges are idempotent by construction (composite PK +
//     ON CONFLICT DO NOTHING): POST follow twice is not an error.
//   - Listings are keyset-paginated on (created_at, edge-id) — never
//     OFFSET — so pages stay stable while the graph grows.
//   - Follower/following counts are computed, not denormalized: one
//     source of truth, no counter drift to repair later.
package social

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Field limits. Handles are the strict public identifier; the rest are
// generous-but-bounded so a client cannot store megabytes in a profile.
const (
	MaxDisplayNameLen = 80   // runes
	MaxBioLen         = 160  // runes (the classic constraint)
	MaxAvatarURLLen   = 2048 // bytes
	MaxPageLimit      = 100
	DefaultPageLimit  = 20
)

// handleRe is the full handle grammar: lowercase alnum + underscore,
// 3–20 chars. Checked in Go before the UNIQUE constraint so failures
// are 422s with a clear message instead of 500s.
var handleRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

var (
	// ErrInvalidUserID is returned when a path user id is not a UUID.
	ErrInvalidUserID = errors.New("social: user id is not a valid UUID")
	// ErrUserNotFound is returned when the target user does not exist.
	ErrUserNotFound = errors.New("social: user not found")
	// ErrInvalidHandle is returned when a handle breaks the grammar.
	ErrInvalidHandle = errors.New("social: handle must match [a-z0-9_]{3,20}")
	// ErrHandleTaken is returned when the handle is already claimed.
	ErrHandleTaken = errors.New("social: handle already taken")
	// ErrSelfFollow is returned when a user tries to follow themselves.
	ErrSelfFollow = errors.New("social: cannot follow yourself")
	// ErrInvalidCursor is returned when a pagination cursor is malformed.
	ErrInvalidCursor = errors.New("social: invalid cursor")
	// ErrFieldTooLong is returned when a profile field exceeds its cap.
	ErrFieldTooLong = errors.New("social: profile field too long")
	// ErrBadAvatarURL is returned when avatar_url is not an http(s) URL.
	// Non-http(s) schemes (e.g. javascript:) would be an XSS vector in
	// clients that render the URL, so they are rejected, not sanitized.
	ErrBadAvatarURL = errors.New("social: avatar_url must be an http(s) URL")
)

// Profile is the public identity record returned by the profile
// endpoints. Counts are computed live (see package doc).
type Profile struct {
	UserID         string    `json:"user_id"`
	Handle         *string   `json:"handle"`
	DisplayName    *string   `json:"display_name"`
	Bio            *string   `json:"bio"`
	AvatarURL      *string   `json:"avatar_url"`
	FollowersCount int64     `json:"followers_count"`
	FollowingCount int64     `json:"following_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ProfilePatch is a partial profile update. Each field carries a Set
// flag: absent keys leave the stored value untouched, present keys set
// it — including explicit null, which clears the field. This is what
// makes PATCH partial without a read-modify-write race on "which keys
// did the client send".
type ProfilePatch struct {
	Handle         *string
	HandleSet      bool
	DisplayName    *string
	DisplayNameSet bool
	Bio            *string
	BioSet         bool
	AvatarURL      *string
	AvatarURLSet   bool
}

// FollowItem is one edge in a followers/following listing.
type FollowItem struct {
	ID          string    `json:"id"`
	Handle      *string   `json:"handle"`
	DisplayName *string   `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
	FollowedAt  time.Time `json:"followed_at"`
}

// FollowPage is one keyset-paginated listing page.
type FollowPage struct {
	Items      []FollowItem `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

// ParseUserID validates a path user id into a pgtype.UUID. Malformed
// ids are 400s without ever touching the database.
func ParseUserID(id string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(id); err != nil {
		return pgtype.UUID{}, ErrInvalidUserID
	}
	if !u.Valid {
		return pgtype.UUID{}, ErrInvalidUserID
	}
	return u, nil
}

// ValidateHandle reports whether handle is claimable.
func ValidateHandle(handle string) bool { return handleRe.MatchString(handle) }

// validatePatch checks every set field of a patch: handle grammar,
// rune/byte caps, avatar URL scheme.
func validatePatch(p ProfilePatch) error {
	if p.HandleSet && p.Handle != nil && !ValidateHandle(*p.Handle) {
		return ErrInvalidHandle
	}
	if p.DisplayNameSet && p.DisplayName != nil &&
		utf8.RuneCountInString(*p.DisplayName) > MaxDisplayNameLen {
		return ErrFieldTooLong
	}
	if p.BioSet && p.Bio != nil &&
		utf8.RuneCountInString(*p.Bio) > MaxBioLen {
		return ErrFieldTooLong
	}
	if p.AvatarURLSet && p.AvatarURL != nil {
		if len(*p.AvatarURL) > MaxAvatarURLLen {
			return ErrFieldTooLong
		}
		u, err := url.Parse(*p.AvatarURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return ErrBadAvatarURL
		}
	}
	return nil
}

// Cursor is the opaque keyset cursor for follow listings: the
// (created_at, edge-id) of the last item on the previous page.
// Clients treat it as an opaque string; it is never a database id.
type Cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
}

// EncodeCursor serializes a cursor for the next_cursor field.
func EncodeCursor(t time.Time, id string) string {
	b, _ := json.Marshal(Cursor{T: t.UTC(), ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a client-supplied cursor. Garbage in →
// ErrInvalidCursor, never a database error.
func DecodeCursor(s string) (Cursor, error) {
	var c Cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, ErrInvalidCursor
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, ErrInvalidCursor
	}
	if c.T.IsZero() || c.ID == "" {
		return c, ErrInvalidCursor
	}
	var id pgtype.UUID
	if err := id.Scan(c.ID); err != nil || !id.Valid {
		return c, ErrInvalidCursor
	}
	return c, nil
}

// scanProfile maps one profile row (from the shared SELECT below) into
// a Profile.
func scanProfile(row pgx.Row) (*Profile, error) {
	var (
		p      Profile
		id     pgtype.UUID
		handle pgtype.Text
		disp   pgtype.Text
		bio    pgtype.Text
		avatar pgtype.Text
	)
	err := row.Scan(&id, &handle, &disp, &bio, &avatar,
		&p.FollowersCount, &p.FollowingCount, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.UserID = id.String()
	if handle.Valid {
		p.Handle = &handle.String
	}
	if disp.Valid {
		p.DisplayName = &disp.String
	}
	if bio.Valid {
		p.Bio = &bio.String
	}
	if avatar.Valid {
		p.AvatarURL = &avatar.String
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	return &p, nil
}

// profileSelect is the shared profile read: LEFT JOIN so users without
// a profiles row still resolve (display name falls back to the OIDC
// seed); counts computed from the edge table.
const profileSelect = `
	SELECT u.id,
	       p.handle,
	       COALESCE(p.display_name, u.display_name),
	       p.bio,
	       p.avatar_url,
	       (SELECT count(*) FROM follows WHERE followee_id = u.id),
	       (SELECT count(*) FROM follows WHERE follower_id = u.id),
	       u.created_at,
	       COALESCE(p.updated_at, u.updated_at)
	FROM users u
	LEFT JOIN profiles p ON p.user_id = u.id
	WHERE u.id = $1`

// GetProfile returns the public profile for userID. Unknown users are
// ErrUserNotFound; users without a profiles row get the sparse fallback.
func GetProfile(ctx context.Context, pool *db.Pool, userID pgtype.UUID) (*Profile, error) {
	p, err := scanProfile(pool.QueryRow(ctx, profileSelect, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return p, err
}

// PatchProfile applies a partial profile update for userID, creating the
// profiles row on first use. The read-merge-write runs inside a
// transaction that locks the users row, so concurrent PATCHes from the
// same user serialize instead of losing each other's fields; the handle
// UNIQUE constraint still arbitrates cross-user claim races (loser →
// ErrHandleTaken).
func PatchProfile(ctx context.Context, pool *db.Pool, userID pgtype.UUID, patch ProfilePatch) (*Profile, error) {
	if err := validatePatch(patch); err != nil {
		return nil, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the identity row: the caller is provisioned (their own row),
	// and this serializes their concurrent PATCHes.
	var locked bool
	err = tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}

	// Merge onto the stored row (missing row = all NULL = fresh profile).
	var (
		handle, disp, bio, avatar pgtype.Text
	)
	err = tx.QueryRow(ctx,
		`SELECT handle, display_name, bio, avatar_url FROM profiles WHERE user_id = $1`,
		userID).Scan(&handle, &disp, &bio, &avatar)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if patch.HandleSet {
		handle = textOrNull(patch.Handle)
	}
	if patch.DisplayNameSet {
		disp = textOrNull(patch.DisplayName)
	}
	if patch.BioSet {
		bio = textOrNull(patch.Bio)
	}
	if patch.AvatarURLSet {
		avatar = textOrNull(patch.AvatarURL)
	}

	const upsert = `
		INSERT INTO profiles (user_id, handle, display_name, bio, avatar_url)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			handle       = EXCLUDED.handle,
			display_name = EXCLUDED.display_name,
			bio          = EXCLUDED.bio,
			avatar_url   = EXCLUDED.avatar_url
		RETURNING user_id`
	var returned pgtype.UUID
	err = tx.QueryRow(ctx, upsert, userID, handle, disp, bio, avatar).Scan(&returned)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "profiles_handle_key" {
			return nil, ErrHandleTaken
		}
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return GetProfile(ctx, pool, userID)
}

// textOrNull converts an optional string into pgtype.Text: nil clears
// the column (NULL = "never set"), non-nil sets it.
func textOrNull(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// Follow creates the follower → followee edge. It returns true when the
// edge is new (caller should answer 201) and false when it already
// existed (idempotent retry → 200). Following a missing user is
// ErrUserNotFound (FK); following yourself is ErrSelfFollow (checked
// here and enforced by the DB CHECK as backstop).
func Follow(ctx context.Context, pool *db.Pool, follower, followee pgtype.UUID) (bool, error) {
	if follower == followee {
		return false, ErrSelfFollow
	}
	var createdAt time.Time
	err := pool.QueryRow(ctx,
		`INSERT INTO follows (follower_id, followee_id)
		 VALUES ($1, $2)
		 ON CONFLICT DO NOTHING
		 RETURNING created_at`,
		follower, followee).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // edge already existed: idempotent success
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23503": // FK: followee does not exist
				return false, ErrUserNotFound
			case "23514": // no_self_follow CHECK (backstop)
				return false, ErrSelfFollow
			}
		}
		return false, err
	}
	return true, nil
}

// Unfollow removes the follower → followee edge. It is idempotent:
// removing a non-existent edge still succeeds. The followee must exist,
// otherwise ErrUserNotFound.
func Unfollow(ctx context.Context, pool *db.Pool, follower, followee pgtype.UUID) error {
	if follower == followee {
		return ErrSelfFollow
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT true FROM users WHERE id = $1`, followee).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	_, err := pool.Exec(ctx,
		`DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2`,
		follower, followee)
	return err
}

// listFollows pages one direction of the edge table. direction is
// "followers" (edges pointing AT userID) or "following" (edges FROM
// userID), newest first.
func listFollows(ctx context.Context, pool *db.Pool, userID pgtype.UUID, direction string, limit int, after *Cursor) (*FollowPage, error) {
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}

	var otherCol, whereCol string
	switch direction {
	case "followers":
		otherCol, whereCol = "f.follower_id", "f.followee_id"
	case "following":
		otherCol, whereCol = "f.followee_id", "f.follower_id"
	default:
		return nil, errors.New("social: unknown follow direction")
	}

	// Keyset clause only when paging past the first page.
	keyset := ""
	args := []any{userID}
	if after != nil {
		var edgeID pgtype.UUID
		if err := edgeID.Scan(after.ID); err != nil || !edgeID.Valid {
			return nil, ErrInvalidCursor
		}
		args = append(args, after.T, edgeID)
		keyset = "AND (f.created_at, " + otherCol + ") < ($2, $3)"
	}
	args = append(args, limit+1) // fetch one extra to detect a next page

	query := `
		SELECT u.id, p.handle, p.display_name, p.avatar_url, f.created_at
		FROM follows f
		JOIN users u ON u.id = ` + otherCol + `
		LEFT JOIN profiles p ON p.user_id = u.id
		WHERE ` + whereCol + ` = $1 ` + keyset + `
		ORDER BY f.created_at DESC, ` + otherCol + ` DESC
		LIMIT $` + strconv.Itoa(len(args)) + `
	`
	// NOTE: column names above are fixed by the direction switch; only
	// values are parameterized.

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &FollowPage{Items: []FollowItem{}}
	for rows.Next() {
		var (
			item       FollowItem
			id         pgtype.UUID
			handle     pgtype.Text
			disp       pgtype.Text
			avatar     pgtype.Text
			followedAt time.Time
		)
		if err := rows.Scan(&id, &handle, &disp, &avatar, &followedAt); err != nil {
			return nil, err
		}
		item.ID = id.String()
		if handle.Valid {
			item.Handle = &handle.String
		}
		if disp.Valid {
			// Display name falls back to the OIDC seed when the user
			// has never set a profile name (same rule as GetProfile).
			item.DisplayName = &disp.String
		}
		if avatar.Valid {
			item.AvatarURL = &avatar.String
		}
		item.FollowedAt = followedAt
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(page.Items) > limit {
		// The cursor points at the last RETURNED item (not the
		// lookahead row): the next page starts strictly after it.
		last := page.Items[limit-1]
		page.Items = page.Items[:limit]
		c := EncodeCursor(last.FollowedAt, last.ID)
		page.NextCursor = &c
	}
	return page, nil
}

// ListFollowers pages the users following userID, newest first.
func ListFollowers(ctx context.Context, pool *db.Pool, userID pgtype.UUID, limit int, after *Cursor) (*FollowPage, error) {
	return listFollows(ctx, pool, userID, "followers", limit, after)
}

// ListFollowing pages the users userID follows, newest first.
func ListFollowing(ctx context.Context, pool *db.Pool, userID pgtype.UUID, limit int, after *Cursor) (*FollowPage, error) {
	return listFollows(ctx, pool, userID, "following", limit, after)
}
