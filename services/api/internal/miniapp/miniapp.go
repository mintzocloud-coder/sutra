// SPDX-License-Identifier: AGPL-3.0-or-later
// Package miniapp is the mini-app registry (G3 part 1): the durable
// platform record of which apps exist, who owns them, what permissions
// they declare, and which users installed them.
//
// The registry is the trust anchor of the Telegram-style runtime: the
// sandboxed frame code runs on the developer's own infrastructure, so
// the ONLY thing the platform trusts is this row — its id, its slug,
// its declared permission manifest. The JS bridge (docs/mini-apps.md,
// G3 part 2) denies any capability not declared here.
//
// Rules (all enforced here, not left to clients):
//   - slug is first-come, first-served ([a-z0-9-]{3,40}), immutable
//     after registration (it is the app's public identifier).
//   - the permission manifest is a subset of the bridge allowlist;
//     anything else is rejected at write time AND denied at runtime.
//   - status follows the transition matrix (draft->published,
//     published->suspended, suspended->published, *->archived); there is
//     no draft<-published path and no un-archive.
//   - only published apps are installable and listable in the public
//     directory; archived apps behave as 404 everywhere.
//   - installs are idempotent by construction (composite PK).
package miniapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Statuses of the mini-app lifecycle. See migration 0015 for the
// transition rules; Transitions documents the allowed matrix.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusSuspended = "suspended"
	StatusArchived  = "archived"
)

// Transitions is the allowed status state machine. Archiving is terminal
// and reachable from everywhere; publish covers both first publication
// and resume-from-suspension so the HTTP surface stays small.
var Transitions = map[string][]string{
	StatusDraft:     {StatusPublished, StatusArchived},
	StatusPublished: {StatusSuspended, StatusArchived},
	StatusSuspended: {StatusPublished, StatusArchived},
	StatusArchived:  {},
}

// BridgePermissions is the full JS-bridge capability allowlist — the
// set of permission strings a manifest may declare. The sandboxed frame
// may only invoke bridge calls gated by permissions declared here; the
// bridge denies undeclared ones (docs/mini-apps.md, G3 part 2).
//
// Anything beyond this list is a new bridge API and needs a platform
// release + client update — never ad-hoc per-app string.
var BridgePermissions = map[string]struct{}{
	"camera":        {},
	"microphone":    {},
	"geolocation":   {},
	"notifications": {},
	"payments":      {},
	"storage":       {},
	"share":         {},
	"clipboard":     {},
	"vibration":     {},
	"biometric":     {},
}

// Sentinel errors; handlers map them to the error envelope.
var (
	ErrMiniAppNotFound     = errors.New("mini app not found")
	ErrSlugTaken           = errors.New("slug already taken")
	ErrInvalidSlug         = errors.New("invalid slug")
	ErrInvalidManifest     = errors.New("invalid manifest")
	ErrInvalidField        = errors.New("invalid field")
	ErrBadStatusTransition = errors.New("status transition not allowed")
	ErrNotOwner            = errors.New("not the app developer")
	ErrInstallNotAllowed   = errors.New("app is not installable")
)

var slugRe = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)

const (
	maxNameLen        = 80
	maxDescriptionLen = 500
	maxVersionLen     = 32
	maxURLLen         = 2048
)

// MiniApp is the registry row. DeveloperID is exposed (unlike OIDC
// subjects) because the developer is a public actor on the platform.
type MiniApp struct {
	ID          pgtype.UUID `json:"id"`
	DeveloperID pgtype.UUID `json:"developer_id"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Description *string     `json:"description,omitempty"`
	IconURL     *string     `json:"icon_url,omitempty"`
	EntryURL    string      `json:"entry_url"`
	Manifest    Manifest    `json:"manifest"`
	Version     string      `json:"version"`
	Status      string      `json:"status"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Manifest is the permission manifest. Permissions is the declared
// subset of BridgePermissions; Raw preserves any extra metadata keys
// (e.g. "display" hints) the platform doesn't interpret.
type Manifest struct {
	Permissions []string       `json:"permissions"`
	Raw         map[string]any `json:"-"`
}

// MarshalJSON preserves the full manifest JSON (declared permissions +
// any metadata keys) as stored.
func (m Manifest) MarshalJSON() ([]byte, error) {
	if m.Raw == nil {
		return json.Marshal(map[string]any{"permissions": m.Permissions})
	}
	out := make(map[string]any, len(m.Raw)+1)
	for k, v := range m.Raw {
		out[k] = v
	}
	out["permissions"] = m.Permissions
	return json.Marshal(out)
}

// parseManifest validates the JSON manifest blob: it must be an object,
// "permissions" (when present) must be an array of strings, and every
// permission must be in BridgePermissions. Unknown metadata keys are
// preserved verbatim (forward compatibility).
func parseManifest(raw json.RawMessage) (Manifest, error) {
	if len(raw) == 0 {
		return Manifest{Permissions: []string{}}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest must be a JSON object", ErrInvalidManifest)
	}
	var perms []string
	if praw, ok := obj["permissions"]; ok {
		if err := json.Unmarshal(praw, &perms); err != nil {
			return Manifest{}, fmt.Errorf("%w: permissions must be an array of strings", ErrInvalidManifest)
		}
	}
	seen := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		p = strings.ToLower(strings.TrimSpace(p))
		if _, ok := BridgePermissions[p]; !ok {
			return Manifest{}, fmt.Errorf("%w: unknown permission %q", ErrInvalidManifest, p)
		}
		if _, dup := seen[p]; dup {
			return Manifest{}, fmt.Errorf("%w: duplicate permission %q", ErrInvalidManifest, p)
		}
		seen[p] = struct{}{}
	}
	if perms == nil {
		perms = []string{}
	}
	manifest := Manifest{Permissions: perms, Raw: map[string]any{}}
	for k, v := range obj {
		if k == "permissions" {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			return Manifest{}, fmt.Errorf("%w: invalid value for %q", ErrInvalidManifest, k)
		}
		manifest.Raw[k] = val
	}
	return manifest, nil
}

func validURL(u string) bool {
	return (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) &&
		len(u) <= maxURLLen
}

// Service is the registry; methods map 1:1 to the HTTP surface.
type Service struct{ pool *db.Pool }

// NewService builds the registry on an existing pool.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// RegisterInput is the POST /v1/mini-apps body.
type RegisterInput struct {
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	Description *string         `json:"description"`
	IconURL     *string         `json:"icon_url"`
	EntryURL    string          `json:"entry_url"`
	Manifest    json.RawMessage `json:"manifest"`
	Version     string          `json:"version"`
}

// validateRegister checks every field before any write.
func validateRegister(in RegisterInput) (Manifest, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > maxNameLen {
		return Manifest{}, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidField, maxNameLen)
	}
	if !slugRe.MatchString(in.Slug) {
		return Manifest{}, fmt.Errorf("%w: slug must match [a-z0-9-]{3,40}", ErrInvalidSlug)
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if d == "" || len([]rune(d)) > maxDescriptionLen {
			return Manifest{}, fmt.Errorf("%w: description must be 1-%d characters", ErrInvalidField, maxDescriptionLen)
		}
	}
	if in.IconURL != nil && !validURL(*in.IconURL) {
		return Manifest{}, fmt.Errorf("%w: icon_url must be an http(s) URL", ErrInvalidField)
	}
	if !validURL(in.EntryURL) {
		return Manifest{}, fmt.Errorf("%w: entry_url must be an http(s) URL", ErrInvalidField)
	}
	if in.Version != "" && len(in.Version) > maxVersionLen {
		return Manifest{}, fmt.Errorf("%w: version too long", ErrInvalidField)
	}
	return parseManifest(in.Manifest)
}

// Register creates a draft mini-app. Slugs are first-come: the UNIQUE
// constraint arbitrates races (loser → ErrSlugTaken, surfaced as 409).
func (s *Service) Register(ctx context.Context, developer pgtype.UUID, in RegisterInput) (*MiniApp, error) {
	manifest, err := validateRegister(in)
	if err != nil {
		return nil, err
	}
	version := in.Version
	if version == "" {
		version = "1.0.0"
	}
	manifestJSON, err := manifest.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var app MiniApp
	row := s.pool.QueryRow(ctx, `
		INSERT INTO mini_apps (developer_id, name, slug, description, icon_url, entry_url, manifest, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)
		RETURNING id, developer_id, name, slug, description, icon_url, entry_url, manifest, version, status, created_at, updated_at`,
		developer, strings.TrimSpace(in.Name), in.Slug, in.Description, in.IconURL, in.EntryURL, string(manifestJSON), version)
	if err := scanMiniApp(row, &app); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return &app, nil
}

// Get fetches one app; archived apps behave as not-found everywhere
// (the registry never resurfaces a tombstoned row).
func (s *Service) Get(ctx context.Context, id pgtype.UUID) (*MiniApp, error) {
	var app MiniApp
	row := s.pool.QueryRow(ctx, `
		SELECT id, developer_id, name, slug, description, icon_url, entry_url, manifest, version, status, created_at, updated_at
		FROM mini_apps WHERE id = $1 AND status <> 'archived'`, id)
	if err := scanMiniApp(row, &app); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMiniAppNotFound
		}
		return nil, err
	}
	return &app, nil
}

// UpdateInput is the PATCH /v1/mini-apps/{id} body. Every field is
// optional (presence-flag partial update); nil = leave untouched.
// Slug and status are NOT updatable here — slug is the immutable public
// identifier, status moves via the dedicated transition endpoints.
type UpdateInput struct {
	Name        *string          `json:"name"`
	Description **string         `json:"description"` // double pointer: explicit null clears
	IconURL     **string         `json:"icon_url"`
	EntryURL    *string          `json:"entry_url"`
	Manifest    *json.RawMessage `json:"manifest"`
	Version     *string          `json:"version"`
}

// Update applies an owner-only partial update. Archived apps are
// invisible (404), matching Get.
func (s *Service) Update(ctx context.Context, id, developer pgtype.UUID, in UpdateInput) (*MiniApp, error) {
	app, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if app.DeveloperID != developer {
		return nil, ErrNotOwner
	}
	set := []string{}
	args := []any{}
	n := 0
	add := func(col string, v any) {
		n++
		set = append(set, fmt.Sprintf("%s = $%d", col, n))
		args = append(args, v)
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len([]rune(name)) > maxNameLen {
			return nil, fmt.Errorf("%w: name must be 1-%d characters", ErrInvalidField, maxNameLen)
		}
		add("name", name)
	}
	if in.Description != nil {
		if *in.Description == nil {
			add("description", nil)
		} else {
			d := strings.TrimSpace(**in.Description)
			if d == "" || len([]rune(d)) > maxDescriptionLen {
				return nil, fmt.Errorf("%w: description must be 1-%d characters", ErrInvalidField, maxDescriptionLen)
			}
			add("description", d)
		}
	}
	if in.IconURL != nil {
		if *in.IconURL == nil {
			add("icon_url", nil)
		} else {
			if !validURL(**in.IconURL) {
				return nil, fmt.Errorf("%w: icon_url must be an http(s) URL", ErrInvalidField)
			}
			add("icon_url", **in.IconURL)
		}
	}
	if in.EntryURL != nil {
		if !validURL(*in.EntryURL) {
			return nil, fmt.Errorf("%w: entry_url must be an http(s) URL", ErrInvalidField)
		}
		add("entry_url", *in.EntryURL)
	}
	if in.Manifest != nil {
		manifest, err := parseManifest(*in.Manifest)
		if err != nil {
			return nil, err
		}
		mj, err := manifest.MarshalJSON()
		if err != nil {
			return nil, err
		}
		add("manifest", string(mj))
	}
	if in.Version != nil {
		v := strings.TrimSpace(*in.Version)
		if v == "" || len(v) > maxVersionLen {
			return nil, fmt.Errorf("%w: version must be 1-%d characters", ErrInvalidField, maxVersionLen)
		}
		add("version", v)
	}
	if len(set) == 0 {
		return app, nil // nothing to change: return current row
	}
	n++
	args = append(args, id)
	var updated MiniApp
	row := s.pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE mini_apps SET %s, updated_at = now()
		WHERE id = $%d AND status <> 'archived'
		RETURNING id, developer_id, name, slug, description, icon_url, entry_url, manifest, version, status, created_at, updated_at`,
		strings.Join(set, ", "), n), args...)
	if err := scanMiniApp(row, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMiniAppNotFound
		}
		return nil, err
	}
	return &updated, nil
}

// Transition moves an app through the status state machine (owner-only).
// Publish (draft->published, suspended->published), Suspend
// (published->suspended) and Archive (*->archived) are thin wrappers.
func (s *Service) Transition(ctx context.Context, id, developer pgtype.UUID, to string) (*MiniApp, error) {
	app, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if app.DeveloperID != developer {
		return nil, ErrNotOwner
	}
	allowed := false
	for _, next := range Transitions[app.Status] {
		if next == to {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%w: %s -> %s", ErrBadStatusTransition, app.Status, to)
	}
	var updated MiniApp
	row := s.pool.QueryRow(ctx, `
		UPDATE mini_apps SET status = $1, updated_at = now()
		WHERE id = $2 AND status = $3
		RETURNING id, developer_id, name, slug, description, icon_url, entry_url, manifest, version, status, created_at, updated_at`,
		to, id, app.Status)
	if err := scanMiniApp(row, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost a concurrent transition race — the read-modify-write
			// window is guarded by the status=$3 predicate.
			return nil, fmt.Errorf("%w: app changed state concurrently", ErrBadStatusTransition)
		}
		return nil, err
	}
	return &updated, nil
}

// Directory lists published apps, newest first, keyset-paginated on
// (created_at, id) — never OFFSET. Archived/draft/suspended apps are
// excluded by the partial index.
func (s *Service) Directory(ctx context.Context, limit int, after *Cursor) (*DirectoryPage, error) {
	args := []any{limit + 1}
	where := "WHERE status = 'published'"
	if after != nil {
		where += " AND (created_at, id) < ($2, $3)"
		args = append(args, after.CreatedAt, after.ID)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, developer_id, name, slug, description, icon_url, entry_url, manifest, version, status, created_at, updated_at
		FROM mini_apps %s
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []*MiniApp{}
	for rows.Next() {
		var app MiniApp
		if err := scanMiniApp(rows, &app); err != nil {
			return nil, err
		}
		apps = append(apps, &app)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &DirectoryPage{}
	if len(apps) > limit {
		apps = apps[:limit]
		last := apps[len(apps)-1]
		page.NextCursor = (&Cursor{CreatedAt: last.CreatedAt, ID: last.ID}).Encode()
	}
	page.Apps = apps
	return page, nil
}

// DirectoryPage is the GET /v1/mini-apps response.
type DirectoryPage struct {
	Apps       []*MiniApp `json:"apps"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Install records the user installing a published app. Idempotent: a
// repeat install returns isNew=false (200), a first install isNew=true
// (201). Only published apps are installable — installing a
// draft/suspended (or archived, which 404s at Get) app is 422.
func (s *Service) Install(ctx context.Context, userID, appID pgtype.UUID) (bool, error) {
	app, err := s.Get(ctx, appID)
	if err != nil {
		return false, err
	}
	if app.Status != StatusPublished {
		return false, fmt.Errorf("%w: only published apps can be installed (status=%s)", ErrInstallNotAllowed, app.Status)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO mini_app_installs (user_id, mini_app_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, appID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Uninstall is idempotent: 204 whether or not the edge existed.
func (s *Service) Uninstall(ctx context.Context, userID, appID pgtype.UUID) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM mini_app_installs WHERE user_id = $1 AND mini_app_id = $2`,
		userID, appID)
	return err
}

// Installed lists the user's installed apps (newest install first),
// with full app rows — the client needs entry_url + manifest to boot
// the sandboxed frame. Includes suspended apps (already installed ones
// keep working) but excludes archived ones.
func (s *Service) Installed(ctx context.Context, userID pgtype.UUID) ([]*InstalledApp, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.developer_id, a.name, a.slug, a.description, a.icon_url, a.entry_url, a.manifest, a.version, a.status, a.created_at, a.updated_at, i.installed_at
		FROM mini_app_installs i
		JOIN mini_apps a ON a.id = i.mini_app_id
		WHERE i.user_id = $1 AND a.status <> 'archived'
		ORDER BY i.installed_at DESC, a.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*InstalledApp{}
	for rows.Next() {
		var app MiniApp
		var ia InstalledApp
		cols := []any{&app.ID, &app.DeveloperID, &app.Name, &app.Slug, &app.Description, &app.IconURL, &app.EntryURL, nil, &app.Version, &app.Status, &app.CreatedAt, &app.UpdatedAt, &ia.InstalledAt}
		var manifestJSON []byte
		cols[7] = &manifestJSON
		if err := rows.Scan(cols...); err != nil {
			return nil, err
		}
		m, err := unmarshalManifest(manifestJSON)
		if err != nil {
			return nil, err
		}
		app.Manifest = m
		ia.MiniApp = &app
		out = append(out, &ia)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// InstalledApp pairs the registry row with the install timestamp.
type InstalledApp struct {
	MiniApp     *MiniApp  `json:"app"`
	InstalledAt time.Time `json:"installed_at"`
}

// scanMiniApp scans the canonical 12-column row shape.
func scanMiniApp(row pgx.Row, app *MiniApp) error {
	var manifestJSON []byte
	if err := row.Scan(&app.ID, &app.DeveloperID, &app.Name, &app.Slug,
		&app.Description, &app.IconURL, &app.EntryURL, &manifestJSON,
		&app.Version, &app.Status, &app.CreatedAt, &app.UpdatedAt); err != nil {
		return err
	}
	m, err := unmarshalManifest(manifestJSON)
	if err != nil {
		return err
	}
	app.Manifest = m
	return nil
}

// unmarshalManifest reads a stored manifest blob back into the typed
// form (permissions array + preserved metadata). Stored rows are always
// valid — they passed parseManifest on write — so errors here are
// treated as corruption.
func unmarshalManifest(raw []byte) (Manifest, error) {
	return parseManifest(json.RawMessage(raw))
}

// isUniqueViolation reports Postgres unique-constraint violations
// (23505) without importing pgconn in every caller.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}
