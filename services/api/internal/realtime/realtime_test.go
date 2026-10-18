// SPDX-License-Identifier: AGPL-3.0-or-later
package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// Publisher construction.
// ---------------------------------------------------------------------------

func TestNewPublisherDisabledWithoutToken(t *testing.T) {
	p, err := NewPublisher("http://chat:4000", "", testLog())
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if p.Enabled() {
		t.Fatal("publisher with empty token must be disabled")
	}
	// Disabled publish is a silent no-op — never an error, never a dial.
	if err := p.Publish(context.Background(), Envelope{
		Topics: []string{"user:x"}, Event: EventPostNew, DedupeKey: "k",
		Payload: map[string]string{"a": "b"},
	}); err != nil {
		t.Fatalf("disabled publish: %v", err)
	}
}

func TestNewPublisherRejectsBadURL(t *testing.T) {
	for _, bad := range []string{"", "://nope", "ftp://chat:4000", "http://"} {
		if _, err := NewPublisher(bad, "secret", testLog()); err == nil {
			t.Fatalf("NewPublisher(%q) should fail", bad)
		}
	}
}

func TestNewPublisherAppendsInternalPath(t *testing.T) {
	p, err := NewPublisher("http://chat:4000/", "secret", testLog())
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	hp, ok := p.(*httpPublisher)
	if !ok {
		t.Fatal("expected *httpPublisher")
	}
	if hp.endpoint != "http://chat:4000/internal/events" {
		t.Fatalf("endpoint = %q", hp.endpoint)
	}
}

// ---------------------------------------------------------------------------
// Envelope validation.
// ---------------------------------------------------------------------------

func TestEnvelopeValidation(t *testing.T) {
	valid := Envelope{
		Topics: []string{"user:alice"}, Event: EventPostNew,
		DedupeKey: "post:1", Payload: map[string]string{"a": "b"},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	cases := []Envelope{
		{Event: EventPostNew, DedupeKey: "k"},                                   // no topics
		{Topics: []string{"room:general"}, Event: EventPostNew, DedupeKey: "k"}, // wrong topic shape
		{Topics: []string{"user:"}, Event: EventPostNew, DedupeKey: "k"},        // empty subject
		{Topics: []string{"user:a"}, Event: "post:like", DedupeKey: "k"},        // unknown event
		{Topics: []string{"user:a"}, Event: EventFollowerNew},                   // no dedupe key
	}
	for i, c := range cases {
		if err := c.validate(); err == nil {
			t.Fatalf("case %d should fail validation", i)
		}
	}
}

func TestChunkTopics(t *testing.T) {
	if got := chunkTopics(nil); len(got) != 0 {
		t.Fatalf("nil → %d chunks", len(got))
	}
	mk := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("user:%d", i)
		}
		return out
	}
	if got := chunkTopics(mk(MaxTopicsPerCall)); len(got) != 1 {
		t.Fatalf("exactly-max → %d chunks", len(got))
	}
	got := chunkTopics(mk(MaxTopicsPerCall + 1))
	if len(got) != 2 || len(got[0]) != MaxTopicsPerCall || len(got[1]) != 1 {
		t.Fatalf("max+1 chunked wrong: %v", []int{len(got[0]), len(got[1])})
	}
}

// ---------------------------------------------------------------------------
// HTTP publish against a fake chat server.
// ---------------------------------------------------------------------------

type capturedCall struct {
	auth     string
	envelope Envelope
}

func fakeChat(t *testing.T, status int) (*httptest.Server, *[]capturedCall) {
	t.Helper()
	var calls []capturedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		var env Envelope
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("bad body: %v", err)
		}
		calls = append(calls, capturedCall{auth: r.Header.Get("Authorization"), envelope: env})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestPublishHappyPath(t *testing.T) {
	srv, calls := fakeChat(t, http.StatusAccepted)
	p, err := NewPublisher(srv.URL, "s3cr3t", testLog())
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	env := Envelope{
		Topics:    []string{"user:alice-sub", "user:bob-sub"},
		Event:     EventFollowerNew,
		DedupeKey: "follow:1:2",
		Payload:   FollowerNewPayload{FollowerID: "1", CreatedAt: time.Now().UTC()},
	}
	if err := p.Publish(context.Background(), env); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	c := (*calls)[0]
	if c.auth != "Bearer s3cr3t" {
		t.Fatalf("auth = %q", c.auth)
	}
	if c.envelope.Event != EventFollowerNew || c.envelope.DedupeKey != "follow:1:2" ||
		len(c.envelope.Topics) != 2 {
		t.Fatalf("envelope mangled: %+v", c.envelope)
	}
	// Payload round-trips as a generic map through JSON.
	pl, ok := c.envelope.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T", c.envelope.Payload)
	}
	if pl["follower_id"] != "1" {
		t.Fatalf("payload = %+v", pl)
	}
}

func TestPublishServerError(t *testing.T) {
	srv, _ := fakeChat(t, http.StatusInternalServerError)
	p, _ := NewPublisher(srv.URL, "s3cr3t", testLog())
	err := p.Publish(context.Background(), Envelope{
		Topics: []string{"user:a"}, Event: EventPostNew, DedupeKey: "k",
		Payload: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestPublishChunksLargeBatches(t *testing.T) {
	srv, calls := fakeChat(t, http.StatusAccepted)
	p, _ := NewPublisher(srv.URL, "s3cr3t", testLog())
	topics := make([]string, MaxTopicsPerCall+100)
	for i := range topics {
		topics[i] = fmt.Sprintf("user:sub-%d", i)
	}
	if err := p.Publish(context.Background(), Envelope{
		Topics: topics, Event: EventPostNew, DedupeKey: "post:big",
		Payload: map[string]string{},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 chunked calls, got %d", len(*calls))
	}
	for _, c := range *calls {
		if len(c.envelope.Topics) > MaxTopicsPerCall {
			t.Fatalf("chunk too large: %d", len(c.envelope.Topics))
		}
		if c.envelope.DedupeKey != "post:big" {
			t.Fatal("dedupe key must ride on every chunk")
		}
	}
}

func TestPublishInvalidEnvelopeNeverDials(t *testing.T) {
	srv, calls := fakeChat(t, http.StatusAccepted)
	p, _ := NewPublisher(srv.URL, "s3cr3t", testLog())
	err := p.Publish(context.Background(), Envelope{Event: EventPostNew}) // no topics
	if err == nil {
		t.Fatal("expected validation error")
	}
	if len(*calls) != 0 {
		t.Fatal("invalid envelope must not hit the network")
	}
}

// ---------------------------------------------------------------------------
// Integration tests (real PostgreSQL + fake chat server).
// ---------------------------------------------------------------------------

var rtTestSeq atomic.Int64

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping realtime integration test")
	}
	pool, err := db.Open(context.Background(), dsn, 5, testLog())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func rtUser(t *testing.T, pool *db.Pool, name string) (pgtype.UUID, string) {
	t.Helper()
	subject := fmt.Sprintf("rt-test-%d-%d", time.Now().UnixNano(), rtTestSeq.Add(1))
	u, err := users.Provision(context.Background(), pool, subject, name)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil || !id.Valid {
		t.Fatalf("bad id %q", u.ID)
	}
	return id, subject
}

func TestFollowerSubjects(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	author, _ := rtUser(t, pool, "author")
	f1, s1 := rtUser(t, pool, "f1")
	f2, s2 := rtUser(t, pool, "f2")
	for _, f := range []pgtype.UUID{f1, f2} {
		if _, err := social.Follow(ctx, pool, f, author); err != nil {
			t.Fatalf("follow: %v", err)
		}
	}
	subs, err := FollowerSubjects(ctx, pool, author, 10000)
	if err != nil {
		t.Fatalf("FollowerSubjects: %v", err)
	}
	got := map[string]bool{}
	for _, s := range subs {
		got[s] = true
	}
	if !got[s1] || !got[s2] || len(subs) != 2 {
		t.Fatalf("subjects = %v", subs)
	}
	// Celebrity: above the threshold → nil, no realtime fan-out.
	subs, err = FollowerSubjects(ctx, pool, author, 1)
	if err != nil {
		t.Fatalf("FollowerSubjects: %v", err)
	}
	if subs != nil {
		t.Fatalf("celebrity should yield nil subjects, got %v", subs)
	}
}

func TestPublishPostNewEndToEnd(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	// Clean setup: one author, one follower.
	authorID, _ := rtUser(t, pool, "poster")
	followerRow, fsub := rtUser(t, pool, "fan")
	if _, err := social.Follow(ctx, pool, followerRow, authorID); err != nil {
		t.Fatalf("follow: %v", err)
	}

	post, err := posts.NewService(pool, 10000).Create(ctx, authorID, "hello realtime", nil)
	if err != nil {
		t.Fatalf("create post: %v", err)
	}

	srv, calls := fakeChat(t, http.StatusAccepted)
	rt, err := NewPublisher(srv.URL, "tok", testLog())
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := PublishPostNew(ctx, pool, post, 10000, rt); err != nil {
		t.Fatalf("PublishPostNew: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	env := (*calls)[0].envelope
	if env.Event != EventPostNew {
		t.Fatalf("event = %q", env.Event)
	}
	if env.DedupeKey != "post:"+post.ID {
		t.Fatalf("dedupe = %q", env.DedupeKey)
	}
	if len(env.Topics) != 1 || env.Topics[0] != "user:"+fsub {
		t.Fatalf("topics = %v (want user:%s)", env.Topics, fsub)
	}
	pl, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T", env.Payload)
	}
	if pl["post_id"] != post.ID || pl["author_id"] != authorID.String() || pl["text"] != "hello realtime" {
		t.Fatalf("payload = %+v", pl)
	}
}

func TestPublishFollowerNewEndToEnd(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	follower, _ := rtUser(t, pool, "fan")
	followee, followeeSub := rtUser(t, pool, "star")

	srv, calls := fakeChat(t, http.StatusAccepted)
	rt, err := NewPublisher(srv.URL, "tok", testLog())
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := PublishFollowerNew(ctx, pool, follower, followee, rt); err != nil {
		t.Fatalf("PublishFollowerNew: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(*calls))
	}
	env := (*calls)[0].envelope
	if env.Event != EventFollowerNew {
		t.Fatalf("event = %q", env.Event)
	}
	if len(env.Topics) != 1 || env.Topics[0] != "user:"+followeeSub {
		t.Fatalf("topics = %v (want user:%s)", env.Topics, followeeSub)
	}
	if env.DedupeKey != "follow:"+follower.String()+":"+followee.String() {
		t.Fatalf("dedupe = %q", env.DedupeKey)
	}
	pl, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T", env.Payload)
	}
	if pl["follower_id"] != follower.String() {
		t.Fatalf("payload = %+v", pl)
	}
}

func TestPublishHelpersDisabledAreNoOps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	author, _ := rtUser(t, pool, "a")
	follower, _ := rtUser(t, pool, "f")
	post, err := posts.NewService(pool, 10000).Create(ctx, author, "x", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	disabled, _ := NewPublisher("http://chat:4000", "", testLog())
	// No TEST chat server needed: disabled must never dial.
	if err := PublishPostNew(ctx, pool, post, 10000, disabled); err != nil {
		t.Fatalf("PublishPostNew disabled: %v", err)
	}
	if err := PublishFollowerNew(ctx, pool, follower, author, disabled); err != nil {
		t.Fatalf("PublishFollowerNew disabled: %v", err)
	}
}
