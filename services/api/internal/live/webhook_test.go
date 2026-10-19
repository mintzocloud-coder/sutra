// SPDX-License-Identifier: AGPL-3.0-or-later
package live

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// signWebhook builds a LiveKit-style webhook token the way
// livekit-server's WebhookReceiver does: HS256 over
// header.payload with iss=<apiKey> and the sha256 claim binding the
// exact body bytes (standard base64).
func signWebhook(t *testing.T, apiKey, apiSecret string, body []byte, mutate func(map[string]any)) string {
	t.Helper()
	sum := sha256.Sum256(body)
	claims := map[string]any{
		"iss":    apiKey,
		"sub":    apiKey,
		"nbf":    time.Now().Add(-time.Minute).Unix(),
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
	}
	if mutate != nil {
		mutate(claims)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pl := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(apiSecret))
	mac.Write([]byte(header + "." + pl))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return apiKey + " " + header + "." + pl + "." + sig
}

func TestVerifyWebhookTokenOK(t *testing.T) {
	body := []byte(`{"event":"room_finished","room":{"name":"sutra:abc"}}`)
	auth := signWebhook(t, "devkey", "devsecret", body, nil)
	if err := verifyWebhookToken(auth, body, "devkey", "devsecret"); err != nil {
		t.Fatalf("valid webhook rejected: %v", err)
	}
}

func TestVerifyWebhookTokenRejects(t *testing.T) {
	body := []byte(`{"event":"room_finished","room":{"name":"sutra:abc"}}`)
	cases := map[string]func(string) string{
		"wrong secret": func(auth string) string {
			return signWebhook(t, "devkey", "othersecret", body, nil)
		},
		"wrong key half": func(auth string) string {
			return "wrongkey " + strings.SplitN(auth, " ", 2)[1]
		},
		"tampered body": func(auth string) string {
			return auth // body checked separately below
		},
		"expired": func(auth string) string {
			return signWebhook(t, "devkey", "devsecret", body, func(c map[string]any) {
				c["exp"] = time.Now().Add(-time.Hour).Unix()
			})
		},
		"future nbf": func(auth string) string {
			return signWebhook(t, "devkey", "devsecret", body, func(c map[string]any) {
				c["nbf"] = time.Now().Add(time.Hour).Unix()
				c["exp"] = time.Now().Add(2 * time.Hour).Unix()
			})
		},
		"wrong sha256 claim": func(auth string) string {
			return signWebhook(t, "devkey", "devsecret", body, func(c map[string]any) {
				c["sha256"] = base64.StdEncoding.EncodeToString(make([]byte, 32))
			})
		},
		"malformed header": func(string) string { return "not-a-token" },
		"empty":            func(string) string { return "" },
	}
	for name, mk := range cases {
		auth := signWebhook(t, "devkey", "devsecret", body, nil)
		checkBody := body
		if name == "tampered body" {
			checkBody = []byte(`{"event":"room_finished","room":{"name":"sutra:EVIL"}}`)
		}
		if err := verifyWebhookToken(mk(auth), checkBody, "devkey", "devsecret"); err == nil {
			t.Fatalf("%s: webhook accepted, want rejection", name)
		}
	}
	// Disabled seam: empty keypair never verifies.
	if err := verifyWebhookToken(signWebhook(t, "k", "s", body, nil), body, "", ""); err == nil {
		t.Fatal("empty keypair: webhook accepted, want rejection")
	}
}

func TestParseSFURoomID(t *testing.T) {
	id, err := parseSFURoomID("sutra:11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("id = %s", id.String())
	}
	for _, bad := range []string{"", "sutra:", "test-room", "sutra:not-a-uuid", "SUTRA:11111111-1111-1111-1111-111111111111"} {
		if _, err := parseSFURoomID(bad); err == nil {
			t.Fatalf("parse(%q): accepted, want rejection", bad)
		}
	}
}
