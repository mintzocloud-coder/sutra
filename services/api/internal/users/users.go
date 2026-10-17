// SPDX-License-Identifier: AGPL-3.0-or-later
// Package users owns the users table: the service's identity record.
//
// The OIDC subject (from the verified JWT) is the stable identity key.
// Rows are created lazily on the first authenticated call
// ("just-in-time provisioning") — no separate signup step is needed for
// OIDC users, which is the standard pattern for services behind an
// identity provider. The display name is seeded from ID-token claims when
// available; the public handle is claimed via PATCH /v1/me/profile and
// lives on the profiles table (see internal/social).
package users

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
)

// User is the API's identity record for one authenticated subject.
type User struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	DisplayName *string   `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ErrEmptySubject is returned when provisioning is attempted without an
// identity. Callers must never let this happen: RequireAuth guarantees a
// subject for every request that reaches a /v1/* handler.
var ErrEmptySubject = errors.New("users: cannot provision user with empty subject")

// displayNameFromClaims seeds the display name from ID-token claims.
// OIDC profile claims are the honest source ("name" first, then
// "preferred_username"); unknown claim shapes never crash provisioning —
// they just mean "no display name yet". Nil claims (auth bypass mode)
// also yield no display name.
func displayNameFromClaims(c *oidc.Claims) string {
	if c == nil {
		return ""
	}
	for _, key := range []string{"name", "preferred_username"} {
		if v, ok := c.Extra[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// Provision returns the user row for subject, creating it on first sight.
// It is safe under concurrency: the INSERT carries ON CONFLICT (subject)
// DO NOTHING, so racing replicas converge on a single row instead of
// failing with duplicate-key errors. The caller-visible display name is
// only a seed for new rows — an existing row's stored name is never
// overwritten here (renames go through the profile endpoint).
func Provision(ctx context.Context, pool *db.Pool, subject, displayName string) (*User, error) {
	if subject == "" {
		return nil, ErrEmptySubject
	}

	// NULLIF keeps the column NULL (rather than empty string) when we
	// have no name to seed: NULL = "never set", which the profile
	// endpoint can distinguish later.
	const insert = `
		INSERT INTO users (subject, display_name)
		VALUES ($1, NULLIF($2, ''))
		ON CONFLICT (subject) DO NOTHING
		RETURNING id, subject, display_name, created_at, updated_at`

	var (
		u    User
		id   pgtype.UUID
		disp pgtype.Text
	)
	err := pool.QueryRow(ctx, insert, subject, displayName).
		Scan(&id, &u.Subject, &disp, &u.CreatedAt, &u.UpdatedAt)
	if err == nil {
		finish(&u, id, disp)
		return &u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Another writer won the race (or the row predates this call):
	// read the row it created.
	const selectBySubject = `
		SELECT id, subject, display_name, created_at, updated_at
		FROM users
		WHERE subject = $1`
	if err := pool.QueryRow(ctx, selectBySubject, subject).
		Scan(&id, &u.Subject, &disp, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	finish(&u, id, disp)
	return &u, nil
}

// finish converts the pgtype nullables into the User's *string fields.
func finish(u *User, id pgtype.UUID, displayName pgtype.Text) {
	u.ID = id.String()
	if displayName.Valid {
		u.DisplayName = &displayName.String
	}
}
