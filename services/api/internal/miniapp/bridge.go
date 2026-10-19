// SPDX-License-Identifier: AGPL-3.0-or-later
// Sandboxed runtime contract (G3 part 2).
//
// The mini-app frame runs on the DEVELOPER's infrastructure (their own
// hosting, their own JS). The platform therefore treats frame code as
// untrusted: the only trusted input is the registry row (miniapp.go)
// plus a short-lived, HMAC-signed launch token minted by this package.
//
// The contract has three parts:
//
//  1. The JS bridge API surface (BridgeMethods): the dotted method
//     names the platform client injects as window.SutraMiniApp.
//     Every method carries the manifest permission it requires (""
//     = no permission needed). The client-side bridge MUST call
//     MethodAllowed against the app's manifest permissions and deny
//     anything not declared — docs/mini-apps.md documents both sides.
//     Adding a method here is a platform release: it ships with the
//     client update, never ad-hoc per app.
//
//  2. The launch token: binds (user_id, app_id) to a 5-minute HMAC
//     (SHA-256) over a versioned payload. The client injects it into
//     the frame (init data) and the developer backend can verify
//     attribution offline with the same secret. It is NOT a session:
//     it proves "user U installed app A at time T" and nothing more.
//
//  3. Service.Launch: the launch gate — who may boot which app.
//     The owner may always launch (draft preview for testing);
//     everyone else must have the install edge AND the app must not be
//     draft. Archived apps 404 everywhere (registry rule).
package miniapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Sentinel errors; the handler maps them to the error envelope.
var (
	ErrUnknownBridgeMethod = errors.New("unknown bridge method")
	ErrLaunchNotAllowed    = errors.New("mini app cannot be launched by this user")
	ErrLaunchSecretMissing = errors.New("launch secret is not configured")
	ErrLaunchTokenInvalid  = errors.New("launch token is invalid")
	ErrLaunchTokenExpired  = errors.New("launch token has expired")
)

// LaunchTokenTTL is the launch-token lifetime. Five minutes is long
// enough for the client to boot the frame and short enough that a
// leaked token is useless.
const LaunchTokenTTL = 5 * time.Minute

// BridgeMethod is one callable on window.SutraMiniApp. Permission is
// the manifest permission string that gates it ("" = no permission
// needed — safe to call by any app).
type BridgeMethod struct {
	Permission  string
	Description string
}

// BridgeMethods is the FULL client JS API surface. The list is closed:
// unknown method names are rejected by MethodAllowed and must fail
// loudly on the client too.
var BridgeMethods = map[string]BridgeMethod{
	"init.getData":              {Permission: "", Description: "platform-injected init data (user, app, theme, signed launch token)"},
	"storage.getItem":           {Permission: "storage", Description: "read a platform-backed key-value entry scoped to (user, app)"},
	"storage.setItem":           {Permission: "storage", Description: "write a platform-backed key-value entry scoped to (user, app); 5MB/app cap enforced client-side"},
	"storage.removeItem":        {Permission: "storage", Description: "delete a platform-backed key-value entry"},
	"camera.capture":            {Permission: "camera", Description: "capture a single photo; the user sees a system capture sheet, never a silent snapshot"},
	"microphone.startListening": {Permission: "microphone", Description: "start audio capture; client shows the recording indicator and requires foreground"},
	"microphone.stopListening":  {Permission: "microphone", Description: "stop audio capture"},
	"geolocation.getPosition":   {Permission: "geolocation", Description: "one-shot device position; client shows the location disclosure sheet"},
	"notifications.subscribe":   {Permission: "notifications", Description: "register this app's push channel; OS permission prompt owned by the client"},
	"payments.initiate":         {Permission: "payments", Description: "start a platform payment sheet (UPI/cards); the frame never touches raw card data"},
	"share.invoke":              {Permission: "share", Description: "open the OS share sheet with title/text/url"},
	"clipboard.writeText":       {Permission: "clipboard", Description: "write text to the clipboard; user gesture required"},
	"vibration.vibrate":         {Permission: "vibration", Description: "haptic pulse (pattern in ms, capped by the client)"},
	"biometric.authenticate":    {Permission: "biometric", Description: "device biometric check (face/fingerprint); the frame only learns pass/fail"},
}

// MethodAllowed reports whether an app with the given manifest may
// invoke the named bridge method. Unknown methods are an error, not a
// deny: a typo'd method name must fail loudly so developers notice in
// testing rather than shipping a silently-broken app.
func MethodAllowed(m Manifest, method string) (bool, error) {
	mdef, ok := BridgeMethods[method]
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownBridgeMethod, method)
	}
	if mdef.Permission == "" {
		return true, nil
	}
	for _, p := range m.Permissions {
		if p == mdef.Permission {
			return true, nil
		}
	}
	return false, nil
}

// launchTokenPayload is the signed body: v1 tag for future rotation,
// the bound identities, and the Unix expiry.
type launchTokenPayload struct {
	V   int    `json:"v"`
	UID string `json:"uid"`
	AID string `json:"aid"`
	Exp int64  `json:"exp"`
}

const launchTokenVersion = "v1"

// MintLaunchToken signs (userID, appID) for ttl. The caller MUST have
// checked launch eligibility (Service.Launch) — minting is pure crypto.
func MintLaunchToken(secret []byte, userID, appID pgtype.UUID, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	if len(secret) == 0 {
		return "", time.Time{}, ErrLaunchSecretMissing
	}
	expiresAt = time.Now().Add(ttl)
	raw, err := json.Marshal(launchTokenPayload{
		V:   1,
		UID: userID.String(),
		AID: appID.String(),
		Exp: expiresAt.Unix(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(launchTokenVersion + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return launchTokenVersion + "." + body + "." + sig, expiresAt, nil
}

// VerifyLaunchToken checks the signature and expiry and returns the
// bound identities. It is deliberately strict: wrong version, bad
// base64, bad MAC, malformed payload, or non-UUID ids are all invalid.
func VerifyLaunchToken(secret []byte, token string) (userID, appID pgtype.UUID, err error) {
	if len(secret) == 0 {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchSecretMissing
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != launchTokenVersion {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	var p launchTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.V != 1 {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	if time.Now().Unix() > p.Exp {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenExpired
	}
	if err := userID.Scan(p.UID); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	if err := appID.Scan(p.AID); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, ErrLaunchTokenInvalid
	}
	return userID, appID, nil
}

// Launch is the result of the launch gate: the registry row to boot
// plus the signed token the client injects into the frame.
type Launch struct {
	App         *MiniApp  `json:"app"`
	LaunchToken string    `json:"launch_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Launch is the server-side launch gate (POST /v1/mini-apps/{id}/launch).
// Rules:
//   - secret missing → ErrLaunchSecretMissing (fail closed).
//   - archived → ErrMiniAppNotFound (registry rule).
//   - the owner may launch in any status (draft preview for testing).
//   - anyone else needs the install edge; draft apps are not launchable
//     by non-owners. Suspended apps keep working for installed users
//     (matches Installed()'s semantics).
func (s *Service) Launch(ctx context.Context, userID, appID pgtype.UUID, secret []byte) (*Launch, error) {
	if len(secret) == 0 {
		return nil, ErrLaunchSecretMissing
	}
	app, err := s.Get(ctx, appID)
	if err != nil {
		return nil, err
	}
	if app.DeveloperID != userID {
		if app.Status == StatusDraft {
			return nil, ErrLaunchNotAllowed
		}
		var n int
		if err := s.pool.QueryRow(ctx, `
			SELECT 1 FROM mini_app_installs
			WHERE user_id = $1 AND mini_app_id = $2`, userID, appID).Scan(&n); err != nil {
			return nil, ErrLaunchNotAllowed
		}
	}
	token, exp, err := MintLaunchToken(secret, userID, appID, LaunchTokenTTL)
	if err != nil {
		return nil, err
	}
	return &Launch{App: app, LaunchToken: token, ExpiresAt: exp}, nil
}
