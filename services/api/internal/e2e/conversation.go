// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrConversationNotFound is returned when no e2e_conversations row
// exists for the room. Callers (and clients) must treat "unknown" as
// NOT E2E — never claim E2E on a conversation where it is not enabled.
var ErrConversationNotFound = errors.New("e2e: conversation not found")

// ConversationE2EStatus is the API view of one e2e_conversations row.
// It carries no key material and no message content — just the flag.
type ConversationE2EStatus struct {
	RoomID     string     `json:"room_id"`
	E2EEnabled bool       `json:"e2e_enabled"`
	EnabledAt  *time.Time `json:"enabled_at,omitempty"`
}

// E2EStatus reads the per-conversation E2E flag for roomID. The chat
// service is the writer (it owns rooms in Wave 1 and produces the
// evidence); the Go API serves this read-only. Unknown room ->
// ErrConversationNotFound.
func (s *Service) E2EStatus(ctx context.Context, roomID string) (*ConversationE2EStatus, error) {
	const q = `SELECT room_id, e2e_enabled, enabled_at
		FROM e2e_conversations WHERE room_id = $1`
	var st ConversationE2EStatus
	var enabledAt pgtype.Timestamptz
	if err := s.pool.QueryRow(ctx, q, roomID).Scan(&st.RoomID, &st.E2EEnabled, &enabledAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, err
	}
	if enabledAt.Valid {
		t := enabledAt.Time
		st.EnabledAt = &t
	}
	return &st, nil
}
