// SPDX-License-Identifier: AGPL-3.0-or-later
// Opaque keyset cursor for the campaign listing: (created_at, id). The
// client never builds it by hand — it only round-trips the next_cursor
// string. Malformed cursors are a 400 (fail loudly on client bugs,
// never silently reset the list).
package ads

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// AdsCursor is a decoded campaign-listing position.
type AdsCursor struct {
	CreatedAt time.Time
	ID        pgtype.UUID
}

// Encode serialises the cursor to an opaque base64url string.
func (c *AdsCursor) Encode() string {
	id := c.ID.String()
	return base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("%d|%s", c.CreatedAt.UnixNano(), id)))
}

// DecodeAdsCursor parses a cursor the client round-tripped.
func DecodeAdsCursor(s string) (AdsCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return AdsCursor{}, ErrInvalidCampaignCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return AdsCursor{}, ErrInvalidCampaignCursor
	}
	var nanos int64
	if _, err := fmt.Sscanf(parts[0], "%d", &nanos); err != nil {
		return AdsCursor{}, ErrInvalidCampaignCursor
	}
	var id pgtype.UUID
	if err := id.Scan(parts[1]); err != nil {
		return AdsCursor{}, ErrInvalidCampaignCursor
	}
	return AdsCursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
