// SPDX-License-Identifier: AGPL-3.0-or-later
package live

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// decodeClaims unwraps the payload of a MintToken result without any
// JWT library: the mint must be stdlib-verifiable to stay SDK-free.
func decodeClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not 3-part JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload not base64url: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	return claims
}

func testRoom(status string) *Room {
	var id, host pgtype.UUID
	_ = id.Scan("11111111-1111-1111-1111-111111111111")
	_ = host.Scan("22222222-2222-2222-2222-222222222222")
	r := &Room{ID: id, HostID: host, Title: "smoke", Kind: KindLivestream, Status: status}
	r.LiveKitRoomName = roomNameFor(id)
	return r
}

func enabledService() *Service {
	return NewService(nil, "devkey", "devsecret", "ws://localhost:7880", 60)
}

func TestMintTokenGrantsViewer(t *testing.T) {
	svc := enabledService()
	token, exp, err := svc.MintToken(testRoom(StatusLive), "subject-1", "Viewer", RoleViewer)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if time.Until(exp) > 61*time.Minute {
		t.Fatalf("ttl too long: %v", time.Until(exp))
	}
	c := decodeClaims(t, token)
	if c["iss"] != "devkey" {
		t.Fatalf("iss = %v, want devkey", c["iss"])
	}
	if c["sub"] != "subject-1" {
		t.Fatalf("sub = %v, want subject-1", c["sub"])
	}
	video, ok := c["video"].(map[string]any)
	if !ok {
		t.Fatalf("no video claim: %v", c)
	}
	if video["roomJoin"] != true {
		t.Fatalf("viewer cannot join: %v", video)
	}
	if video["room"] != "sutra:11111111-1111-1111-1111-111111111111" {
		t.Fatalf("room scope = %v, want the sutra:<id> wire name", video["room"])
	}
	if video["canPublish"] != false {
		t.Fatalf("viewer got canPublish=true: %v", video)
	}
	if video["canSubscribe"] != true {
		t.Fatalf("viewer cannot subscribe: %v", video)
	}
}

func TestMintTokenGrantsHost(t *testing.T) {
	svc := enabledService()
	token, _, err := svc.MintToken(testRoom(StatusLive), "subject-1", "Host", RoleHost)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	video := decodeClaims(t, token)["video"].(map[string]any)
	if video["canPublish"] != true {
		t.Fatalf("host cannot publish: %v", video)
	}
}

func TestMintTokenRefusals(t *testing.T) {
	svc := enabledService()
	if _, _, err := svc.MintToken(testRoom(StatusLive), "s", "", "admin"); err != ErrBadRole {
		t.Fatalf("bad role: got %v, want ErrBadRole", err)
	}
	if _, _, err := svc.MintToken(testRoom(StatusEnded), "s", "", RoleViewer); err != ErrRoomEnded {
		t.Fatalf("ended room: got %v, want ErrRoomEnded", err)
	}
	disabled := NewService(nil, "", "", "", 0)
	if _, _, err := disabled.MintToken(testRoom(StatusLive), "s", "", RoleViewer); err != ErrLiveDisabled {
		t.Fatalf("disabled: got %v, want ErrLiveDisabled", err)
	}
}

func TestTitleValidation(t *testing.T) {
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxTitleLen+1), "ok\xffbad"} {
		if err := validateTitle(bad); err != ErrBadTitle {
			t.Fatalf("title %q: got %v, want ErrBadTitle", bad, err)
		}
	}
	if err := validateTitle("  Friday match  "); err != nil {
		t.Fatalf("good title rejected: %v", err)
	}
}

func TestUUIDEqual(t *testing.T) {
	var a, b, c pgtype.UUID
	_ = a.Scan("11111111-1111-1111-1111-111111111111")
	_ = b.Scan("11111111-1111-1111-1111-111111111111")
	_ = c.Scan("33333333-3333-3333-3333-333333333333")
	if !uuidEqual(a, b) {
		t.Fatal("equal UUIDs compared unequal")
	}
	if uuidEqual(a, c) {
		t.Fatal("different UUIDs compared equal")
	}
}
