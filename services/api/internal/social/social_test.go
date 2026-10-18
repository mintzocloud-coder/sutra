// SPDX-License-Identifier: AGPL-3.0-or-later
package social

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/oidc"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

// Pure unit tests: no database needed.

func TestValidateHandle(t *testing.T) {
	valid := []string{"abc", "a1_", "_x9", "smoke_a", strings.Repeat("a", 20)}
	for _, h := range valid {
		if !ValidateHandle(h) {
			t.Fatalf("ValidateHandle(%q) = false, want true", h)
		}
	}
	invalid := []string{
		"", "ab", strings.Repeat("a", 21), // length
		"Abc", "ABC", "smoke-a", "smoke.a", "smoke a", "smoke@a", // charset/case
		"café", "a/b", // non-ascii / path chars
	}
	for _, h := range invalid {
		if ValidateHandle(h) {
			t.Fatalf("ValidateHandle(%q) = true, want false", h)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := "123e4567-e89b-12d3-a456-426614174000"
	s := EncodeCursor(now, id)
	c, err := DecodeCursor(s)
	if err != nil {
		t.Fatalf("DecodeCursor(EncodeCursor(...)): %v", err)
	}
	if !c.T.Equal(now) || c.ID != id {
		t.Fatalf("round trip = %+v, want t=%v id=%q", c, now, id)
	}

	for _, bad := range []string{
		"", "!!!not-base64!!!",
		encodeB64("hello"),                                          // valid base64, not JSON
		encodeB64(`{"t":null,"id":"x"}`),                            // zero time
		encodeB64(`{"t":"x","id":""}`),                              // empty id
		encodeB64(`{"t":"2026-01-01T00:00:00Z","id":"not-a-uuid"}`), // bad uuid
	} {
		if _, err := DecodeCursor(bad); err != ErrInvalidCursor {
			t.Fatalf("DecodeCursor(%q) = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

// encodeB64 base64url-encodes s (for building malformed cursors).
func encodeB64(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestValidatePatch(t *testing.T) {
	str := func(s string) *string { return &s }
	good := ProfilePatch{
		Handle: str("smoke_a"), HandleSet: true,
		DisplayName: str("Alice"), DisplayNameSet: true,
		Bio: str("hello"), BioSet: true,
		AvatarURL: str("https://cdn.example/x.png"), AvatarURLSet: true,
	}
	if err := validatePatch(good); err != nil {
		t.Fatalf("validatePatch(good) = %v", err)
	}

	cases := []struct {
		name  string
		patch ProfilePatch
		want  error
	}{
		{"bad handle", ProfilePatch{Handle: str("UPPER"), HandleSet: true}, ErrInvalidHandle},
		{"short handle", ProfilePatch{Handle: str("ab"), HandleSet: true}, ErrInvalidHandle},
		{"cleared handle ok", ProfilePatch{Handle: nil, HandleSet: true}, nil},
		{"long display name", ProfilePatch{DisplayName: str(strings.Repeat("x", 81)), DisplayNameSet: true}, ErrFieldTooLong},
		{"long bio", ProfilePatch{Bio: str(strings.Repeat("x", 161)), BioSet: true}, ErrFieldTooLong},
		{"emoji bio counts runes", ProfilePatch{Bio: str(strings.Repeat("🎉", 160)), BioSet: true}, nil},
		{"javascript avatar", ProfilePatch{AvatarURL: str("javascript:alert(1)"), AvatarURLSet: true}, ErrBadAvatarURL},
		{"relative avatar", ProfilePatch{AvatarURL: str("/x.png"), AvatarURLSet: true}, ErrBadAvatarURL},
		{"http avatar ok", ProfilePatch{AvatarURL: str("http://cdn.example/x.png"), AvatarURLSet: true}, nil},
		{"cleared avatar ok", ProfilePatch{AvatarURL: nil, AvatarURLSet: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePatch(tc.patch); err != tc.want {
				t.Fatalf("validatePatch = %v, want %v", err, tc.want)
			}
		})
	}
}

// Integration tests against a real PostgreSQL (migrations applied).
// Run when TEST_DATABASE_URL is set; otherwise skip cleanly.

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping social integration test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func uniqueSubject(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return t.Name() + "-" + hex.EncodeToString(b[:])
}

// uniqueHandle returns a handle unique to this test run (the shared test
// database persists across runs, so fixed handles would collide).
func uniqueHandle(t *testing.T, prefix string) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	h := prefix + "_" + hex.EncodeToString(b[:])
	if len(h) > 20 {
		t.Fatalf("uniqueHandle prefix too long: %q", prefix)
	}
	return h
}

// provisionUser creates a fresh user row for tests and returns its UUID.
func provisionUser(t *testing.T, pool *db.Pool, name string) string {
	t.Helper()
	u, err := users.Provision(context.Background(), pool, uniqueSubject(t), name)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return u.ID
}

func strptr(s string) *string { return &s }

func TestPatchProfileIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	uid := provisionUser(t, pool, "Alice")
	userID, err := ParseUserID(uid)
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}

	// First patch creates the row: handle claimed, name overridden.
	aliceHandle := uniqueHandle(t, "alice")
	p, err := PatchProfile(ctx, pool, userID, ProfilePatch{
		Handle: strptr(aliceHandle), HandleSet: true,
		Bio: strptr("hello world"), BioSet: true,
	})
	if err != nil {
		t.Fatalf("PatchProfile (first): %v", err)
	}
	if p.UserID != uid {
		t.Fatalf("user_id = %q, want %q", p.UserID, uid)
	}
	if p.Handle == nil || *p.Handle != aliceHandle {
		t.Fatalf("handle = %v, want %q", p.Handle, aliceHandle)
	}
	if p.Bio == nil || *p.Bio != "hello world" {
		t.Fatalf("bio = %v, want 'hello world'", p.Bio)
	}
	if p.FollowersCount != 0 || p.FollowingCount != 0 {
		t.Fatalf("counts = %d/%d, want 0/0", p.FollowersCount, p.FollowingCount)
	}

	// Partial patch: only display_name — handle and bio preserved.
	p2, err := PatchProfile(ctx, pool, userID, ProfilePatch{
		DisplayName: strptr("Alice A"), DisplayNameSet: true,
	})
	if err != nil {
		t.Fatalf("PatchProfile (partial): %v", err)
	}
	if p2.Handle == nil || *p2.Handle != aliceHandle {
		t.Fatalf("partial patch lost handle: %v", p2.Handle)
	}
	if p2.Bio == nil || *p2.Bio != "hello world" {
		t.Fatalf("partial patch lost bio: %v", p2.Bio)
	}
	if p2.DisplayName == nil || *p2.DisplayName != "Alice A" {
		t.Fatalf("display_name = %v, want 'Alice A'", p2.DisplayName)
	}

	// Explicit null clears the bio; handle stays.
	p3, err := PatchProfile(ctx, pool, userID, ProfilePatch{Bio: nil, BioSet: true})
	if err != nil {
		t.Fatalf("PatchProfile (clear bio): %v", err)
	}
	if p3.Bio != nil {
		t.Fatalf("bio = %q, want nil (cleared)", *p3.Bio)
	}
	if p3.Handle == nil || *p3.Handle != aliceHandle {
		t.Fatalf("clearing bio lost handle: %v", p3.Handle)
	}

	// Another user claiming the same handle → 409-class error.
	uid2 := provisionUser(t, pool, "Bob")
	userID2, _ := ParseUserID(uid2)
	_, err = PatchProfile(ctx, pool, userID2, ProfilePatch{
		Handle: strptr(aliceHandle), HandleSet: true,
	})
	if err != ErrHandleTaken {
		t.Fatalf("duplicate handle claim = %v, want ErrHandleTaken", err)
	}

	// Invalid handle never reaches the database.
	_, err = PatchProfile(ctx, pool, userID, ProfilePatch{
		Handle: strptr("UPPER"), HandleSet: true,
	})
	if err != ErrInvalidHandle {
		t.Fatalf("bad handle = %v, want ErrInvalidHandle", err)
	}

	// GetProfile on a user with no profiles row: sparse fallback, OIDC
	// display-name seed visible, handle nil.
	uid3 := provisionUser(t, pool, "Carol")
	userID3, _ := ParseUserID(uid3)
	sparse, err := GetProfile(ctx, pool, userID3)
	if err != nil {
		t.Fatalf("GetProfile (sparse): %v", err)
	}
	if sparse.Handle != nil {
		t.Fatalf("sparse handle = %q, want nil", *sparse.Handle)
	}
	if sparse.DisplayName == nil || *sparse.DisplayName != "Carol" {
		t.Fatalf("sparse display_name = %v, want OIDC seed 'Carol'", sparse.DisplayName)
	}

	// Unknown user → ErrUserNotFound.
	unknown, _ := ParseUserID("123e4567-e89b-12d3-a456-426614174000")
	if _, err := GetProfile(ctx, pool, unknown); err != ErrUserNotFound {
		t.Fatalf("GetProfile(unknown) = %v, want ErrUserNotFound", err)
	}
	if _, err := PatchProfile(ctx, pool, unknown, ProfilePatch{}); err != ErrUserNotFound {
		t.Fatalf("PatchProfile(unknown) = %v, want ErrUserNotFound", err)
	}
}

func TestHandleClaimRaceIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Ten users racing to claim the same handle: exactly one wins.
	raceHandle := uniqueHandle(t, "racewin")
	const racers = 10
	wins := make([]bool, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := provisionUser(t, pool, "Racer")
			userID, _ := ParseUserID(uid)
			_, err := PatchProfile(ctx, pool, userID, ProfilePatch{
				Handle: strptr(raceHandle), HandleSet: true,
			})
			wins[i] = err == nil
		}(i)
	}
	wg.Wait()
	n := 0
	for _, w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("handle claim race: %d winners, want exactly 1", n)
	}
}

func TestFollowIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	a := provisionUser(t, pool, "A")
	b := provisionUser(t, pool, "B")
	aid, _ := ParseUserID(a)
	bid, _ := ParseUserID(b)

	// New edge → true (201-class).
	isNew, err := Follow(ctx, pool, aid, bid)
	if err != nil || !isNew {
		t.Fatalf("Follow = (%v, %v), want (true, nil)", isNew, err)
	}
	// Retry → false, no error (idempotent).
	isNew, err = Follow(ctx, pool, aid, bid)
	if err != nil || isNew {
		t.Fatalf("Follow retry = (%v, %v), want (false, nil)", isNew, err)
	}

	// Counts on both profiles.
	pa, _ := GetProfile(ctx, pool, aid)
	pb, _ := GetProfile(ctx, pool, bid)
	if pa.FollowingCount != 1 || pa.FollowersCount != 0 {
		t.Fatalf("A counts = %d/%d, want 0 followers / 1 following", pa.FollowersCount, pa.FollowingCount)
	}
	if pb.FollowersCount != 1 || pb.FollowingCount != 0 {
		t.Fatalf("B counts = %d/%d, want 1 follower / 0 following", pb.FollowersCount, pb.FollowingCount)
	}

	// Self-follow rejected.
	if _, err := Follow(ctx, pool, aid, aid); err != ErrSelfFollow {
		t.Fatalf("self-follow = %v, want ErrSelfFollow", err)
	}
	if err := Unfollow(ctx, pool, aid, aid); err != ErrSelfFollow {
		t.Fatalf("self-unfollow = %v, want ErrSelfFollow", err)
	}

	// Missing followee → ErrUserNotFound both directions.
	unknown, _ := ParseUserID("123e4567-e89b-12d3-a456-426614174000")
	if _, err := Follow(ctx, pool, aid, unknown); err != ErrUserNotFound {
		t.Fatalf("follow unknown = %v, want ErrUserNotFound", err)
	}
	if err := Unfollow(ctx, pool, aid, unknown); err != ErrUserNotFound {
		t.Fatalf("unfollow unknown = %v, want ErrUserNotFound", err)
	}

	// Unfollow: edge gone, counts back to zero; retry idempotent.
	if err := Unfollow(ctx, pool, aid, bid); err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	if err := Unfollow(ctx, pool, aid, bid); err != nil {
		t.Fatalf("Unfollow retry: %v", err)
	}
	pa, _ = GetProfile(ctx, pool, aid)
	pb, _ = GetProfile(ctx, pool, bid)
	if pa.FollowingCount != 0 || pb.FollowersCount != 0 {
		t.Fatalf("counts after unfollow = %d/%d, want 0/0", pa.FollowingCount, pb.FollowersCount)
	}
}

func TestFollowListsIntegration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	center := provisionUser(t, pool, "Center")
	cid, _ := ParseUserID(center)

	// Five followers, created in order (microsecond sleeps keep
	// created_at distinct for a deterministic newest-first check).
	var ids []string
	for _, name := range []string{"F1", "F2", "F3", "F4", "F5"} {
		fid := provisionUser(t, pool, name)
		fuuid, _ := ParseUserID(fid)
		if _, err := Follow(ctx, pool, fuuid, cid); err != nil {
			t.Fatalf("Follow: %v", err)
		}
		ids = append(ids, fid)
		time.Sleep(2 * time.Millisecond)
	}

	// Page 1: newest two first.
	page1, err := ListFollowers(ctx, pool, cid, 2, nil)
	if err != nil {
		t.Fatalf("ListFollowers p1: %v", err)
	}
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("p1 = %d items next=%v, want 2 items + cursor", len(page1.Items), page1.NextCursor)
	}
	if page1.Items[0].ID != ids[4] || page1.Items[1].ID != ids[3] {
		t.Fatalf("p1 order = %q,%q, want newest first (%q,%q)",
			page1.Items[0].ID, page1.Items[1].ID, ids[4], ids[3])
	}

	// Page 2 via cursor.
	after, err := DecodeCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	page2, err := ListFollowers(ctx, pool, cid, 2, &after)
	if err != nil {
		t.Fatalf("ListFollowers p2: %v", err)
	}
	if len(page2.Items) != 2 || page2.NextCursor == nil {
		t.Fatalf("p2 = %d items next=%v, want 2 items + cursor", len(page2.Items), page2.NextCursor)
	}

	// Page 3: last item, no next cursor.
	after2, _ := DecodeCursor(*page2.NextCursor)
	page3, err := ListFollowers(ctx, pool, cid, 2, &after2)
	if err != nil {
		t.Fatalf("ListFollowers p3: %v", err)
	}
	if len(page3.Items) != 1 || page3.NextCursor != nil {
		t.Fatalf("p3 = %d items next=%v, want 1 item + nil cursor", len(page3.Items), page3.NextCursor)
	}
	if page3.Items[0].ID != ids[0] {
		t.Fatalf("p3 item = %q, want oldest %q", page3.Items[0].ID, ids[0])
	}

	// Following direction: center follows F1 and F2.
	f1, _ := ParseUserID(ids[0])
	f2, _ := ParseUserID(ids[1])
	if _, err := Follow(ctx, pool, cid, f1); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := Follow(ctx, pool, cid, f2); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	following, err := ListFollowing(ctx, pool, cid, 10, nil)
	if err != nil {
		t.Fatalf("ListFollowing: %v", err)
	}
	if len(following.Items) != 2 || following.NextCursor != nil {
		t.Fatalf("following = %d items next=%v, want 2 + nil", len(following.Items), following.NextCursor)
	}
	if following.Items[0].ID != ids[1] {
		t.Fatalf("following newest = %q, want %q", following.Items[0].ID, ids[1])
	}

	// Items carry profile data: claim a handle and check it surfaces.
	f1Handle := uniqueHandle(t, "fone")
	if _, err := PatchProfile(ctx, pool, f1, ProfilePatch{Handle: strptr(f1Handle), HandleSet: true}); err != nil {
		t.Fatalf("PatchProfile: %v", err)
	}
	following2, err := ListFollowing(ctx, pool, cid, 10, nil)
	if err != nil {
		t.Fatalf("ListFollowing: %v", err)
	}
	found := false
	for _, it := range following2.Items {
		if it.ID == ids[0] {
			found = true
			if it.Handle == nil || *it.Handle != f1Handle {
				t.Fatalf("item handle = %v, want %q", it.Handle, f1Handle)
			}
			if it.FollowedAt.IsZero() {
				t.Fatal("item followed_at is zero")
			}
		}
	}
	if !found {
		t.Fatal("F1 missing from following list")
	}

	// Bad cursor → ErrInvalidCursor; unknown user lists still 404 via
	// GetProfile (tested at HTTP level below).
	bad := Cursor{}
	if _, err := ListFollowers(ctx, pool, cid, 2, &bad); err != ErrInvalidCursor {
		t.Fatalf("bad cursor = %v, want ErrInvalidCursor", err)
	}
}

// HTTP-level tests: handlers behind the real auth middleware (bypass),
// with the same mux patterns as cmd/api/main.go so {id} path values are
// exercised.

func testServer(t *testing.T, pool *db.Pool, subject string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := middleware.RequireAuth(oidc.NewVerifier("", "", ""), log,
		middleware.AuthOptions{Bypass: true, BypassSubject: subject})

	mux := http.NewServeMux()
	allow := func(methods []string, h http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			for _, m := range methods {
				if r.Method == m {
					h.ServeHTTP(w, r)
					return
				}
			}
			middleware.MethodNotAllowedHandler(methods...)(w, r)
		}
	}
	mux.Handle("/v1/me/profile", allow([]string{http.MethodPatch}, ProfileHandler(pool, log)))
	mux.Handle("/v1/users/{id}", allow([]string{http.MethodGet}, UserHandler(pool, log)))
	mux.Handle("/v1/users/{id}/follow", FollowHandler(pool, FollowHooks{}, log))
	mux.Handle("/v1/users/{id}/followers", allow([]string{http.MethodGet}, FollowersHandler(pool, log)))
	mux.Handle("/v1/users/{id}/following", allow([]string{http.MethodGet}, FollowingHandler(pool, log)))

	srv := httptest.NewServer(auth(mux))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	var v map[string]any
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, v
}

func TestSocialHTTPIntegration(t *testing.T) {
	pool := testPool(t)
	subA, subB := uniqueSubject(t), uniqueSubject(t)
	srvA := testServer(t, pool, subA)
	srvB := testServer(t, pool, subB)

	// A claims a handle via PATCH.
	aliceHandle := uniqueHandle(t, "httpalice")
	status, body := doJSON(t, "PATCH", srvA.URL+"/v1/me/profile",
		`{"handle":"`+aliceHandle+`","display_name":"Alice"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH profile = %d (%v), want 200", status, body)
	}
	if body["handle"] != aliceHandle {
		t.Fatalf("handle = %v, want %q", body["handle"], aliceHandle)
	}
	idA, _ := body["user_id"].(string)
	if idA == "" {
		t.Fatal("PATCH response has no user_id")
	}

	// GET on the PATCH-only route → 405 envelope.
	status, body = doJSON(t, "GET", srvA.URL+"/v1/me/profile", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/me/profile = %d, want 405", status)
	}
	if body["error"] == nil {
		t.Fatalf("405 body not an envelope: %v", body)
	}

	// Unknown JSON field → 400 (strict API: typos fail loudly).
	status, body = doJSON(t, "PATCH", srvA.URL+"/v1/me/profile", `{"handl":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH unknown field = %d, want 400", status)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "unknown_field" {
		t.Fatalf("400 code = %v, want unknown_field", body)
	}

	// B claims the same handle → 409 handle_taken envelope.
	status, body = doJSON(t, "PATCH", srvB.URL+"/v1/me/profile", `{"handle":"`+aliceHandle+`"}`)
	if status != http.StatusConflict {
		t.Fatalf("duplicate handle = %d (%v), want 409", status, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "handle_taken" {
		t.Fatalf("409 code = %v, want handle_taken", body)
	}

	// B claims its own handle; grab its id.
	bobHandle := uniqueHandle(t, "httpbob")
	status, body = doJSON(t, "PATCH", srvB.URL+"/v1/me/profile", `{"handle":"`+bobHandle+`"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH profile B = %d (%v), want 200", status, body)
	}
	idB, _ := body["user_id"].(string)

	// GET public profile: no subject leaked.
	status, body = doJSON(t, "GET", srvA.URL+"/v1/users/"+idB, "")
	if status != http.StatusOK {
		t.Fatalf("GET user = %d (%v), want 200", status, body)
	}
	if body["handle"] != bobHandle {
		t.Fatalf("profile handle = %v, want %q", body["handle"], bobHandle)
	}
	if _, ok := body["subject"]; ok {
		t.Fatalf("profile leaks subject: %v", body)
	}
	if body["followers_count"] != float64(0) {
		t.Fatalf("followers_count = %v, want 0", body["followers_count"])
	}

	// Malformed UUID → 400 envelope; unknown UUID → 404 envelope.
	status, _ = doJSON(t, "GET", srvA.URL+"/v1/users/not-a-uuid", "")
	if status != http.StatusBadRequest {
		t.Fatalf("GET bad uuid = %d, want 400", status)
	}
	status, body = doJSON(t, "GET", srvA.URL+"/v1/users/123e4567-e89b-12d3-a456-426614174000", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown user = %d, want 404", status)
	}

	// A follows B: 201 first, 200 on retry (idempotent).
	status, _ = doJSON(t, "POST", srvA.URL+"/v1/users/"+idB+"/follow", "")
	if status != http.StatusCreated {
		t.Fatalf("POST follow = %d, want 201", status)
	}
	status, _ = doJSON(t, "POST", srvA.URL+"/v1/users/"+idB+"/follow", "")
	if status != http.StatusOK {
		t.Fatalf("POST follow retry = %d, want 200", status)
	}

	// A cannot follow A: 422.
	status, body = doJSON(t, "POST", srvA.URL+"/v1/users/"+idA+"/follow", "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("POST self-follow = %d (%v), want 422", status, body)
	}

	// B's followers list contains A.
	status, body = doJSON(t, "GET", srvB.URL+"/v1/users/"+idB+"/followers?limit=10", "")
	if status != http.StatusOK {
		t.Fatalf("GET followers = %d (%v), want 200", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != idA {
		t.Fatalf("followers = %v, want [A]", body["items"])
	}
	if body["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want null on last page", body["next_cursor"])
	}

	// A's following list contains B; counts updated.
	status, body = doJSON(t, "GET", srvA.URL+"/v1/users/"+idA+"/following", "")
	if status != http.StatusOK {
		t.Fatalf("GET following = %d, want 200", status)
	}
	items, _ = body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != idB {
		t.Fatalf("following = %v, want [B]", body["items"])
	}
	status, body = doJSON(t, "GET", srvA.URL+"/v1/users/"+idB, "")
	if body["followers_count"] != float64(1) {
		t.Fatalf("B followers_count = %v, want 1 (status %d)", body["followers_count"], status)
	}

	// PUT on follow route → 405 with Allow.
	status, _ = doJSON(t, "PUT", srvA.URL+"/v1/users/"+idB+"/follow", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT follow = %d, want 405", status)
	}

	// Unfollow: 204, then followers empty.
	status, _ = doJSON(t, "DELETE", srvA.URL+"/v1/users/"+idB+"/follow", "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE follow = %d, want 204", status)
	}
	status, body = doJSON(t, "GET", srvB.URL+"/v1/users/"+idB+"/followers", "")
	items, _ = body["items"].([]any)
	if status != http.StatusOK || len(items) != 0 {
		t.Fatalf("followers after unfollow = %v (status %d), want []", body["items"], status)
	}

	// Bad cursor → 400 envelope.
	status, _ = doJSON(t, "GET", srvA.URL+"/v1/users/"+idA+"/followers?cursor=bogus", "")
	if status != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400", status)
	}
}
