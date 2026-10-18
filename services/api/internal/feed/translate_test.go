// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mintzocloud-coder/sutra/services/api/internal/posts"
)

func testTranslatorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubTranslateServer returns an httptest server that mimics the AI
// service's /translate: it echoes "[<target>] <text>" (the stub backend
// marker) and records the request headers for assertions.
func stubTranslateServer(t *testing.T, got *translateRequest, gotAuth, gotSubject *string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/translate" {
			http.NotFound(w, r)
			return
		}
		var req translateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		*got = req
		*gotAuth = r.Header.Get("Authorization")
		*gotSubject = r.Header.Get("X-Sutra-Subject")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Text:   "[" + req.Target + "] " + req.Text,
			Source: "en",
			Target: req.Target,
			Cached: false,
			Model:  "stub",
		})
	}))
}

func TestNewTranslatorDisabled(t *testing.T) {
	tr, err := NewTranslator("http://ai:8000", "", testTranslatorLogger())
	if err != nil {
		t.Fatalf("disabled translator must not error: %v", err)
	}
	if tr.Enabled() {
		t.Fatal("translator with empty credential must be disabled")
	}
}

func TestNewTranslatorBadURL(t *testing.T) {
	for _, bad := range []string{"://nope", "", "no-scheme-here"} {
		if _, err := NewTranslator(bad, "secret", testTranslatorLogger()); err == nil {
			t.Fatalf("NewTranslator(%q) with credential set must fail", bad)
		}
	}
}

func TestTranslateSuccess(t *testing.T) {
	var got translateRequest
	var gotAuth, gotSubject string
	srv := stubTranslateServer(t, &got, &gotAuth, &gotSubject, http.StatusOK)
	defer srv.Close()

	tr, err := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	if err != nil {
		t.Fatalf("NewTranslator: %v", err)
	}
	translated, source, err := tr.Translate(context.Background(), "user-sub-1", "namaste duniya", "hi")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if translated != "[hi] namaste duniya" {
		t.Fatalf("translated = %q, want stub marker", translated)
	}
	if source != "en" {
		t.Fatalf("source = %q, want en", source)
	}
	if got.Text != "namaste duniya" || got.Source != "auto" || got.Target != "hi" {
		t.Fatalf("request body = %+v, want text+auto+hi", got)
	}
	if gotAuth != "Bearer svc-secret" {
		t.Fatalf("Authorization = %q, want the service credential", gotAuth)
	}
	if gotSubject != "user-sub-1" {
		t.Fatalf("X-Sutra-Subject = %q, want user-sub-1", gotSubject)
	}
}

func TestTranslateNon200(t *testing.T) {
	var got translateRequest
	var gotAuth, gotSubject string
	srv := stubTranslateServer(t, &got, &gotAuth, &gotSubject, http.StatusServiceUnavailable)
	defer srv.Close()

	tr, _ := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	if _, _, err := tr.Translate(context.Background(), "s", "text", "hi"); err == nil {
		t.Fatal("non-2xx from the AI service must surface as an error")
	}
}

func TestTranslateDisabled(t *testing.T) {
	tr, _ := NewTranslator("http://ai:8000", "", testTranslatorLogger())
	if _, _, err := tr.Translate(context.Background(), "s", "text", "hi"); err == nil {
		t.Fatal("disabled translator must return an error")
	}
	var nilTr *Translator
	if nilTr.Enabled() {
		t.Fatal("nil translator must report disabled")
	}
}

func TestDecorateItemsSuccess(t *testing.T) {
	var got translateRequest
	var gotAuth, gotSubject string
	srv := stubTranslateServer(t, &got, &gotAuth, &gotSubject, http.StatusOK)
	defer srv.Close()

	tr, _ := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	items := []FeedItem{
		{Post: posts.Post{ID: "p1", Text: "hello world"}},
		{Post: posts.Post{ID: "p2", Text: "second post"}},
	}
	out := tr.decorateItems(context.Background(), "user-sub-1", "gu", items)
	if out[0].TranslatedText == nil || *out[0].TranslatedText != "[gu] hello world" {
		t.Fatalf("item 0 not translated: %+v", out[0])
	}
	if out[0].TranslatedFrom == nil || *out[0].TranslatedFrom != "en" {
		t.Fatalf("item 0 translated_from missing: %+v", out[0])
	}
	if out[1].TranslatedText == nil || !strings.HasPrefix(*out[1].TranslatedText, "[gu]") {
		t.Fatalf("item 1 not translated: %+v", out[1])
	}
	// Originals are untouched.
	if out[0].Text != "hello world" {
		t.Fatalf("original text mutated: %q", out[0].Text)
	}
}

func TestDecorateItemsBestEffortOnOutage(t *testing.T) {
	var got translateRequest
	var gotAuth, gotSubject string
	srv := stubTranslateServer(t, &got, &gotAuth, &gotSubject, http.StatusServiceUnavailable)
	defer srv.Close()

	tr, _ := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	items := []FeedItem{{Post: posts.Post{ID: "p1", Text: "hello world"}}}
	out := tr.decorateItems(context.Background(), "user-sub-1", "gu", items)
	// Best-effort: no error escapes, no panic, item keeps original text.
	if len(out) != 1 || out[0].Text != "hello world" {
		t.Fatalf("page lost on translation outage: %+v", out)
	}
	if out[0].TranslatedText != nil {
		t.Fatalf("TranslatedText set despite outage: %q", *out[0].TranslatedText)
	}
}

func TestDecorateItemsDisabledIsNoop(t *testing.T) {
	tr, _ := NewTranslator("http://ai:8000", "", testTranslatorLogger())
	items := []FeedItem{{Post: posts.Post{ID: "p1", Text: "hello world"}}}
	out := tr.decorateItems(context.Background(), "s", "gu", items)
	if out[0].TranslatedText != nil {
		t.Fatal("disabled translator must not touch items")
	}
	var nilTr *Translator
	out = nilTr.decorateItems(context.Background(), "s", "gu", items)
	if out[0].TranslatedText != nil {
		t.Fatal("nil translator must not touch items")
	}
}

func TestDecorateItemsSameLanguageNoop(t *testing.T) {
	// Backend echoes the text unchanged (same-language no-op): the
	// fields must stay unset rather than echoing noise.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{
			Text: "hello world", Source: "en", Target: "en",
		})
	}))
	defer srv.Close()

	tr, _ := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	items := []FeedItem{{Post: posts.Post{ID: "p1", Text: "hello world"}}}
	out := tr.decorateItems(context.Background(), "s", "en", items)
	if out[0].TranslatedText != nil || out[0].TranslatedFrom != nil {
		t.Fatalf("same-language echo must not set translation fields: %+v", out[0])
	}
}

func TestDecorateItemsRespectsPageBudget(t *testing.T) {
	// Each call blocks 300ms; with the 6s page budget, a page of items
	// finishes well inside it — but the mechanism under test is that a
	// hung backend cannot stall the feed: use a context that is already
	// near-deadline to prove decorateItems returns promptly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translateResponse{Text: "x", Source: "en", Target: "hi"})
	}))
	defer srv.Close()

	tr, _ := NewTranslator(srv.URL, "svc-secret", testTranslatorLogger())
	items := make([]FeedItem, 5)
	for i := range items {
		items[i] = FeedItem{Post: posts.Post{ID: "p", Text: "hello"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := tr.decorateItems(ctx, "s", "hi", items)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("decorateItems did not respect the deadline: %v", elapsed)
	}
	if len(out) != 5 {
		t.Fatalf("items lost on deadline: %d", len(out))
	}
}

func TestSupportedTranslateTargets(t *testing.T) {
	for _, want := range []string{"hi", "gu", "ta", "bn", "te", "kn", "ml", "mr", "pa", "or", "as", "ur", "ne", "en"} {
		if !SupportedTranslateTargets[want] {
			t.Fatalf("target %q missing", want)
		}
	}
	for _, bad := range []string{"", "xx", "EN", "hindi", "zh"} {
		if SupportedTranslateTargets[bad] {
			t.Fatalf("target %q must be rejected", bad)
		}
	}
}
