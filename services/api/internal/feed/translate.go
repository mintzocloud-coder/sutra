// SPDX-License-Identifier: AGPL-3.0-or-later
package feed

// E4 language bridge (feed side): GET /v1/feed/home?translate=xx.
//
// The `translate` query parameter is a convenience layer over the AI
// service's POST /translate endpoint (services/ai, E4 part 1): the
// caller reads their home feed and gets each post's body translated
// into the requested language alongside the original. It is
// deliberately best-effort:
//
//   - translation is a read-time decoration, never part of feed
//     assembly — a downed or slow AI service degrades to the original
//     text, never a 503;
//   - per-page translation work is bounded (translatePageBudget);
//     items past the budget keep their original text;
//   - an invalid `translate` value is a client bug: 400
//     `invalid_translate_target` (same spirit as malformed cursors).
//
// Item JSON: `translated_text` / `translated_from` appear only when a
// translation actually happened (omitempty). Same-language results are
// dropped: the Python service no-ops those, and echoing them back as
// "translated" would be noise.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SupportedTranslateTargets is the ISO 639-1 set the feed accepts for
// ?translate=. It MUST stay in sync with SUPPORTED_LANGUAGES in
// services/ai/app/translate.py (keys). Go validates cheaply at the
// edge; Python validates again as the authority.
var SupportedTranslateTargets = map[string]bool{
	"hi": true, "gu": true, "ta": true, "bn": true, "te": true,
	"kn": true, "ml": true, "mr": true, "pa": true, "or": true,
	"as": true, "ur": true, "ne": true, "en": true,
}

// ErrTranslateDisabled reports a Translate call on a disabled
// translator (no shared credential configured).
var ErrTranslateDisabled = errors.New("feed: translation disabled")

// translatePageBudget caps total translation work per feed read. The
// feed read must not wait on a slow model: 100 items translate
// sequentially, so the budget is shared across the whole page and the
// remaining items keep their original text. Newest items are first, so
// the budget is spent where the reader looks first.
const translatePageBudget = 6 * time.Second

// translateRequest is the body POSTed to the AI service's /translate.
type translateRequest struct {
	Text   string `json:"text"`
	Source string `json:"source"` // "auto": Unicode-script detection
	Target string `json:"target"`
}

// translateResponse mirrors the AI service's success envelope.
type translateResponse struct {
	Text   string `json:"text"`
	Source string `json:"source"`
	Target string `json:"target"`
	Cached bool   `json:"cached"`
	Model  string `json:"model"`
}

// Translator calls the AI service's /translate endpoint for feed
// reads. It is a dumb client: auth, validation and caching live in the
// Python service; here we only add the caller's verified subject
// (X-Sutra-Subject, for the Python per-user rate limit) and keep every
// failure from escaping into the feed path.
type Translator struct {
	baseURL    string
	credential string // shared service-to-service secret; empty = disabled
	client     *http.Client
	log        *slog.Logger
}

// NewTranslator builds the feed translator. An empty credential
// disables translation (loud WARN at boot in main.go); a credential
// with an unparseable base URL is a configuration bug — refuse rather
// than serve a silently untranslated feed.
func NewTranslator(baseURL, credential string, log *slog.Logger) (*Translator, error) {
	if credential == "" {
		return &Translator{log: log}, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("feed: invalid AI service URL %q", baseURL)
	}
	return &Translator{
		baseURL:    strings.TrimRight(baseURL, "/"),
		credential: credential,
		client:     &http.Client{Timeout: 8 * time.Second},
		log:        log,
	}, nil
}

// Enabled reports whether translation calls will be attempted.
func (t *Translator) Enabled() bool { return t != nil && t.credential != "" }

// Translate sends one text to the AI service's /translate endpoint and
// returns (translated text, detected source language). Any non-2xx,
// network error, or decode failure is returned as an error — the
// caller (decorateItems) treats all of them as best-effort misses.
func (t *Translator) Translate(ctx context.Context, subject, text, target string) (string, string, error) {
	if !t.Enabled() {
		return "", "", ErrTranslateDisabled
	}
	body, err := json.Marshal(translateRequest{Text: text, Source: "auto", Target: target})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/translate", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.credential)
	req.Header.Set("X-Sutra-Subject", subject)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("feed: translate call failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("feed: translate backend returned %d", resp.StatusCode)
	}
	var out translateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("feed: translate response undecodable: %w", err)
	}
	if out.Text == "" {
		return "", "", errors.New("feed: translate backend returned empty text")
	}
	return out.Text, out.Source, nil
}

// decorateItems translates page item texts best-effort into `target`.
// Nil or disabled translator → items returned untouched. Every failure
// (timeout, AI-service outage, per-item error) is logged once per item
// and the item keeps its original text — translation can never turn a
// healthy feed read into an error.
func (t *Translator) decorateItems(ctx context.Context, subject, target string, items []FeedItem) []FeedItem {
	if !t.Enabled() {
		return items
	}
	ctx, cancel := context.WithTimeout(ctx, translatePageBudget)
	defer cancel()
	for i := range items {
		if items[i].Text == "" {
			continue
		}
		translated, source, err := t.Translate(ctx, subject, items[i].Text, target)
		if err != nil {
			t.log.Warn("feed: translation failed (best-effort; item keeps original text)",
				"post_id", items[i].ID, "target", target, "err", err)
			continue
		}
		if translated == items[i].Text {
			continue // same-language no-op echoed back; not a translation
		}
		items[i].TranslatedText = &translated
		items[i].TranslatedFrom = &source
	}
	return items
}
