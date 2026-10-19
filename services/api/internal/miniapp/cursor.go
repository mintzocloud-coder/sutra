// SPDX-License-Identifier: AGPL-3.0-or-later
// Opaque keyset cursor for the mini-app directory listing: (created_at,
// id). The client never builds it by hand — it only round-trips the
// next_cursor string. Malformed cursors are a 400 (fail loudly on client
// bugs, never silently reset the feed).
package miniapp

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ErrInvalidCursor is returned for malformed directory cursors.
var ErrInvalidCursor = errors.New("invalid cursor")

// Cursor is a decoded directory position.
type Cursor struct {
	CreatedAt time.Time
	ID        pgtype.UUID
}

// Encode serialises the cursor to an opaque base64url string.
func (c *Cursor) Encode() string {
	id := c.ID.String()
	return base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("%d|%s", c.CreatedAt.UnixNano(), id)))
}

// DecodeCursor parses a cursor the client round-tripped.
func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return Cursor{}, ErrInvalidCursor
	}
	var nanos int64
	if _, err := fmt.Sscanf(parts[0], "%d", &nanos); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	var id pgtype.UUID
	if err := id.Scan(parts[1]); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
