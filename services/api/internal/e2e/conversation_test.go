// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// roomSuffix mints a unique room id per test (the shared test DB keeps
// rows between tests).
func roomSuffix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("i3-%s-%d", t.Name(), time.Now().UnixNano())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestConversationE2EStatusUnknown: an unknown room is
// ErrConversationNotFound - never a row, never a silent false. Clients
// must treat unknown as NOT E2E.
func TestConversationE2EStatusUnknown(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	_, err := svc.E2EStatus(context.Background(), "no-such-room-xyz")
	if err != ErrConversationNotFound {
		t.Fatalf("expected ErrConversationNotFound, got %v", err)
	}
}

// TestConversationE2EStatusRead: a chat-written row reads back with the
// flag and timestamp.
func TestConversationE2EStatusRead(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	room := roomSuffix(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO e2e_conversations (room_id, e2e_enabled, enabled_at, first_init_message_id)
		 VALUES ($1, true, now(), $2)`, room, pgtype.UUID{}); err != nil {
		t.Fatalf("insert flag row: %v", err)
	}
	st, err := svc.E2EStatus(ctx, room)
	if err != nil {
		t.Fatalf("E2EStatus: %v", err)
	}
	if st.RoomID != room || !st.E2EEnabled || st.EnabledAt == nil {
		t.Fatalf("bad status: %+v", st)
	}
}

// TestConversationE2EHandlerGrammar: malformed room ids are 400
// invalid_room_id before the service is ever touched (pool == nil is
// fine here). Always runs - no DB needed.
func TestConversationE2EHandlerGrammar(t *testing.T) {
	svc := NewService(nil)
	for _, bad := range []string{"bad!!room", "", "a b", strings.Repeat("x", 65), "../escape"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/conversations/x/e2e", nil)
		req.SetPathValue("room_id", bad)
		w := httptest.NewRecorder()
		ConversationE2EHandler(svc, discardLogger()).ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("room %q: got %d, want 400", bad, w.Code)
		}
	}
	// NOTE: the 64-char boundary (longest well-formed id) is exercised
	// in TestConversationE2EHandlerPaths - it passes grammar and needs a
	// live pool.
}

// TestConversationE2EHandlerPaths: the HTTP surface - 200 on a flagged
// room, 404 conversation_not_found on unknown. DB-backed; skips cleanly
// without TEST_DATABASE_URL (CI is the gate).
func TestConversationE2EHandlerPaths(t *testing.T) {
	pool := testPool(t)
	svc := NewService(pool)
	ctx := context.Background()
	room := roomSuffix(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO e2e_conversations (room_id, e2e_enabled, enabled_at)
		 VALUES ($1, true, now())`, room); err != nil {
		t.Fatalf("insert flag row: %v", err)
	}

	// Boundary: 64 chars is the longest well-formed id - it passes
	// grammar and reaches the service (404 here, no row for it).
	boundary := httptest.NewRequest(http.MethodGet, "/v1/conversations/"+strings.Repeat("a", 64)+"/e2e", nil)
	boundary.SetPathValue("room_id", strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	ConversationE2EHandler(svc, discardLogger()).ServeHTTP(w, boundary)
	if w.Code != http.StatusNotFound {
		t.Fatalf("64-char unknown room: got %d, want 404", w.Code)
	}

	ok := httptest.NewRequest(http.MethodGet, "/v1/conversations/"+room+"/e2e", nil)
	ok.SetPathValue("room_id", room)
	w = httptest.NewRecorder()
	ConversationE2EHandler(svc, discardLogger()).ServeHTTP(w, ok)
	if w.Code != http.StatusOK {
		t.Fatalf("flagged room: got %d, want 200 (%s)", w.Code, w.Body.String())
	}

	miss := httptest.NewRequest(http.MethodGet, "/v1/conversations/no-such-room-xyz/e2e", nil)
	miss.SetPathValue("room_id", "no-such-room-xyz")
	w = httptest.NewRecorder()
	ConversationE2EHandler(svc, discardLogger()).ServeHTTP(w, miss)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown room: got %d, want 404", w.Code)
	}
}
