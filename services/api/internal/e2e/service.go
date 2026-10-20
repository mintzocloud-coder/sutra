// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
)

// Sentinel errors for the service layer. Handlers map these to the
// JSON error envelope.
var (
	// ErrDeviceNotFound is returned when a device label names no row
	// for the calling (or target) user.
	ErrDeviceNotFound = errors.New("e2e: device not found")
	// ErrPrekeyIDConflict is returned when replenishing a prekey id
	// that already exists on the device with DIFFERENT key bytes — a
	// client bug (same id, same key is an idempotent no-op instead).
	ErrPrekeyIDConflict = errors.New("e2e: one-time prekey id already registered with different key bytes")
)

// DirectoryPageCap bounds the device directory: a user has a handful
// of devices; the cap is a backstop, not pagination (devices are
// listed, never searched).
const DirectoryPageCap = 50

// Service is the E2E device-key store. It needs only the pool — no
// config, no external dependencies — which is why newHandler builds it
// directly from pool (unit tests pass pool == nil and the routes stay
// unregistered there, same convention as mediaSvc).
type Service struct {
	pool *db.Pool
}

// NewService builds the device-key service.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// b64 encodes raw key bytes for the wire: standard base64 (with
// padding), exactly as the registration endpoints accept.
func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// Device is the API view of one e2e_devices row. It carries no key
// material beyond counts and ids — the private keys were never stored,
// and even the public keys are only exposed through the directory
// endpoints that need them (Directory / ConsumePrekey), never here.
type Device struct {
	ID               string     `json:"id"`
	DeviceID         string     `json:"device_id"`
	RegistrationID   int        `json:"registration_id"`
	SignedPrekeyID   int        `json:"signed_prekey_id"`
	AvailablePrekeys int        `json:"available_prekeys"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
}

// DevicePublic is the directory view: a Device plus the public key
// material a session initiator needs for X3DH. Everything in here is
// public by design (same threat model as Signal's prekey server).
type DevicePublic struct {
	Device
	IdentityKey     string `json:"identity_key"`      // base64, 32B Ed25519
	SignedPrekey    string `json:"signed_prekey"`     // base64, 32B X25519
	SignedPrekeySig string `json:"signed_prekey_sig"` // base64, 64B Ed25519 sig
}

// OneTimePrekeyOut is a single handed-out one-time prekey.
type OneTimePrekeyOut struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"` // base64, 32B X25519
}

// PrekeyBundle is what GET .../prekey-bundle returns: the device's
// public bundle plus the one consumed one-time prekey. OneTimePrekey
// is nil when the device's queue ran dry — the initiator then falls
// back to signed-prekey-only X3DH (still authenticated, still
// forward-secret from the ratchet onward) and the device should
// replenish.
type PrekeyBundle struct {
	DeviceID        string            `json:"device_id"`
	RegistrationID  int               `json:"registration_id"`
	IdentityKey     string            `json:"identity_key"`
	SignedPrekey    string            `json:"signed_prekey"`
	SignedPrekeyID  int               `json:"signed_prekey_id"`
	SignedPrekeySig string            `json:"signed_prekey_sig"`
	OneTimePrekey   *OneTimePrekeyOut `json:"one_time_prekey"`
	// AvailablePrekeys is the queue depth AFTER this consumption —
	// the initiator can warn the device owner when it runs low.
	AvailablePrekeys int `json:"available_prekeys"`
}

// scanDevice maps the shared device-row projection.
func scanDevice(row pgx.Row, withKeys bool) (*Device, *DevicePublic, error) {
	var (
		d        Device
		id       pgtype.UUID
		pub      DevicePublic
		ikey     []byte
		spk      []byte
		spkSig   []byte
		lastSeen pgtype.Timestamptz
	)
	var err error
	if withKeys {
		err = row.Scan(&id, &d.DeviceID, &d.RegistrationID,
			&d.SignedPrekeyID, &d.AvailablePrekeys,
			&d.CreatedAt, &d.UpdatedAt, &lastSeen,
			&ikey, &spk, &spkSig)
	} else {
		err = row.Scan(&id, &d.DeviceID, &d.RegistrationID,
			&d.SignedPrekeyID, &d.AvailablePrekeys,
			&d.CreatedAt, &d.UpdatedAt, &lastSeen)
	}
	if err != nil {
		return nil, nil, err
	}
	d.ID = id.String()
	if lastSeen.Valid {
		t := lastSeen.Time
		d.LastSeenAt = &t
	}
	if !withKeys {
		return &d, nil, nil
	}
	pub.Device = d
	pub.IdentityKey = b64(ikey)
	pub.SignedPrekey = b64(spk)
	pub.SignedPrekeySig = b64(spkSig)
	return &d, &pub, nil
}

// deviceColumns is the projection shared by the own-device and
// directory queries; available is the live unconsumed-prekey count.
const deviceColumns = `d.id, d.device_id, d.registration_id,
	d.signed_prekey_id,
	(SELECT count(*) FROM e2e_one_time_prekeys k
	 WHERE k.device_id = d.id AND k.consumed_at IS NULL) AS available,
	d.created_at, d.updated_at, d.last_seen_at`

// RegisterDevice verifies and persists a device's key bundle. The
// first registration for (user_id, device_id) INSERTs; a repeat
// registration (same device label — e.g. app reinstall with the same
// label, or a signed-prekey rotation) REPLACES the key material and
// the unconsumed one-time queue, and reports created=false.
//
// Replacement semantics, deliberately: consumed prekey rows are kept
// for audit (their private halves are already dead — either used in a
// session or lost with the old install), unconsumed rows are dropped
// (their private halves are gone with the old install, so handing
// them out would mint sessions nobody can complete), and last_seen_at
// is bumped (the device just checked in).
//
// The whole registration is one transaction: a crash can never leave
// a device row with a half-written queue.
func (s *Service) RegisterDevice(ctx context.Context, userID pgtype.UUID, b KeyBundle) (*Device, bool, error) {
	if err := ValidateBundle(b); err != nil {
		return nil, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	var (
		devUUID pgtype.UUID
		created bool
	)
	// The xmax trick (same as the chat upsert in B4): xmax = 0 means
	// this statement INSERTed; otherwise it hit the conflict path.
	err = tx.QueryRow(ctx, `
		INSERT INTO e2e_devices
			(user_id, device_id, identity_key, signed_prekey,
			 signed_prekey_id, signed_prekey_sig, registration_id,
			 last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (user_id, device_id) DO UPDATE SET
			identity_key = EXCLUDED.identity_key,
			signed_prekey = EXCLUDED.signed_prekey,
			signed_prekey_id = EXCLUDED.signed_prekey_id,
			signed_prekey_sig = EXCLUDED.signed_prekey_sig,
			registration_id = EXCLUDED.registration_id,
			last_seen_at = now()
		RETURNING id, (xmax = 0)`,
		userID, b.DeviceID, b.IdentityKey, b.SignedPrekey,
		b.SignedPrekeyID, b.SignedPrekeySig, b.RegistrationID).
		Scan(&devUUID, &created)
	if err != nil {
		return nil, false, err
	}

	// Drop the unconsumed queue: those private halves are gone with
	// the previous install/rotation. Consumed rows stay for audit.
	if _, err := tx.Exec(ctx, `
		DELETE FROM e2e_one_time_prekeys
		WHERE device_id = $1 AND consumed_at IS NULL`, devUUID); err != nil {
		return nil, false, err
	}

	ids := make([]int32, len(b.OneTimePrekeys))
	keys := make([][]byte, len(b.OneTimePrekeys))
	for i, k := range b.OneTimePrekeys {
		ids[i] = int32(k.ID)
		keys[i] = k.PublicKey
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO e2e_one_time_prekeys (device_id, prekey_id, public_key)
		SELECT $1, unnest($2::int[]), unnest($3::bytea[])`,
		devUUID, ids, keys); err != nil {
		return nil, false, err
	}

	dev, _, err := scanDevice(tx.QueryRow(ctx, `
		SELECT `+deviceColumns+` FROM e2e_devices d WHERE d.id = $1`, devUUID), false)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return dev, created, nil
}

// Devices lists the caller's own devices (no key material — the
// counts and ids are what a device-management UI needs).
func (s *Service) Devices(ctx context.Context, userID pgtype.UUID) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deviceColumns+`
		FROM e2e_devices d WHERE d.user_id = $1
		ORDER BY d.created_at LIMIT $2`, userID, DirectoryPageCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, _, err := scanDevice(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ReplenishPrekeys adds one-time prekeys to the caller's device queue.
// Same id + same key bytes is an idempotent no-op (safe client retry);
// same id + DIFFERENT key bytes is ErrPrekeyIDConflict (a client bug —
// prekey ids must be unique per device). Returns the new available
// count so the client knows when it has restocked enough.
func (s *Service) ReplenishPrekeys(ctx context.Context, userID pgtype.UUID, deviceID string, keys []OneTimePrekey) (int, error) {
	if len(keys) == 0 {
		return 0, fmt.Errorf("%w: need at least 1 one-time prekey", ErrInvalidBundle)
	}
	seen := make(map[int]struct{}, len(keys))
	for i, k := range keys {
		if k.ID < 0 {
			return 0, fmt.Errorf("%w: one_time_prekeys[%d].id %d is negative",
				ErrInvalidBundle, i, k.ID)
		}
		if _, dup := seen[k.ID]; dup {
			return 0, fmt.Errorf("%w: duplicate one_time_prekeys id %d",
				ErrInvalidBundle, k.ID)
		}
		seen[k.ID] = struct{}{}
		if len(k.PublicKey) != X25519PubKeyLen {
			return 0, fmt.Errorf("%w: one_time_prekeys[%d] is %d bytes, want %d",
				ErrInvalidBundle, i, len(k.PublicKey), X25519PubKeyLen)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var devUUID pgtype.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM e2e_devices WHERE user_id = $1 AND device_id = $2`,
		userID, deviceID).Scan(&devUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrDeviceNotFound
	}
	if err != nil {
		return 0, err
	}

	// Conflict check: an id that already exists with different bytes
	// is a client bug; with identical bytes it is a retry no-op.
	existing := map[int][]byte{}
	rows, err := tx.Query(ctx, `
		SELECT prekey_id, public_key FROM e2e_one_time_prekeys
		WHERE device_id = $1`, devUUID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var pid int32
		var pk []byte
		if err := rows.Scan(&pid, &pk); err != nil {
			rows.Close()
			return 0, err
		}
		existing[int(pid)] = pk
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fresh := keys[:0]
	for _, k := range keys {
		if old, ok := existing[k.ID]; ok {
			if string(old) != string(k.PublicKey) {
				return 0, fmt.Errorf("%w: id %d", ErrPrekeyIDConflict, k.ID)
			}
			continue // identical retry — skip
		}
		fresh = append(fresh, k)
	}
	if len(fresh) > 0 {
		ids := make([]int32, len(fresh))
		pks := make([][]byte, len(fresh))
		for i, k := range fresh {
			ids[i] = int32(k.ID)
			pks[i] = k.PublicKey
		}
		// ON CONFLICT DO NOTHING: two racing replenishes with the
		// same id+key converge instead of erroring; the pre-check
		// above already rejected differing bytes.
		if _, err := tx.Exec(ctx, `
			INSERT INTO e2e_one_time_prekeys (device_id, prekey_id, public_key)
			SELECT $1, unnest($2::int[]), unnest($3::bytea[])
			ON CONFLICT (device_id, prekey_id) DO NOTHING`,
			devUUID, ids, pks); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE e2e_devices SET last_seen_at = now() WHERE id = $1`, devUUID); err != nil {
		return 0, err
	}
	var available int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM e2e_one_time_prekeys
		WHERE device_id = $1 AND consumed_at IS NULL`, devUUID).Scan(&available); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return available, nil
}

// DeleteDevice removes the caller's device; the one-time queue
// cascades (migration 0021 ON DELETE CASCADE). 204 on success,
// ErrDeviceNotFound when the label names nothing.
func (s *Service) DeleteDevice(ctx context.Context, userID pgtype.UUID, deviceID string) error {
	ct, err := s.pool.Exec(ctx, `
		DELETE FROM e2e_devices WHERE user_id = $1 AND device_id = $2`,
		userID, deviceID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// Directory returns the public key directory for any user: every
// device with the key material a session initiator needs for X3DH.
// Any authenticated user may read it — initiating a session with
// someone requires their public bundle, exactly like Signal's prekey
// server. Unknown users yield an empty list (no existence oracle
// beyond what GET /v1/users/{id} already gives).
func (s *Service) Directory(ctx context.Context, targetUserID pgtype.UUID) ([]DevicePublic, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deviceColumns+`,
			d.identity_key, d.signed_prekey, d.signed_prekey_sig
		FROM e2e_devices d WHERE d.user_id = $1
		ORDER BY d.created_at LIMIT $2`, targetUserID, DirectoryPageCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DevicePublic{}
	for rows.Next() {
		_, pub, err := scanDevice(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *pub)
	}
	return out, rows.Err()
}

// ConsumePrekey fetches one device's public bundle AND consumes its
// lowest-id available one-time prekey, atomically. The SELECT ...
// FOR UPDATE SKIP LOCKED (backed by the migration 0021 partial index)
// means concurrent initiators never receive the same prekey — each
// one-time prekey is handed out at most once, which is the property
// the X3DH forward secrecy of the first message rests on.
func (s *Service) ConsumePrekey(ctx context.Context, targetUserID pgtype.UUID, deviceID string) (*PrekeyBundle, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var (
		devUUID   pgtype.UUID
		regID     int
		ikey, spk []byte
		spkID     int
		spkSig    []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT id, registration_id, identity_key, signed_prekey,
		       signed_prekey_id, signed_prekey_sig
		FROM e2e_devices WHERE user_id = $1 AND device_id = $2`,
		targetUserID, deviceID).
		Scan(&devUUID, &regID, &ikey, &spk, &spkID, &spkSig)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}

	var (
		pkRowID pgtype.UUID
		pkID    int32
		pkBytes []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT id, prekey_id, public_key FROM e2e_one_time_prekeys
		WHERE device_id = $1 AND consumed_at IS NULL
		ORDER BY prekey_id LIMIT 1 FOR UPDATE SKIP LOCKED`, devUUID).
		Scan(&pkRowID, &pkID, &pkBytes)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Queue dry — the initiator falls back to signed-prekey-only
		// X3DH. Not an error: the device can still receive sessions.
	case err != nil:
		return nil, err
	default:
		if _, err := tx.Exec(ctx, `
			UPDATE e2e_one_time_prekeys SET consumed_at = now() WHERE id = $1`,
			pkRowID); err != nil {
			return nil, err
		}
	}

	var available int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM e2e_one_time_prekeys
		WHERE device_id = $1 AND consumed_at IS NULL`, devUUID).Scan(&available); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	b := &PrekeyBundle{
		DeviceID:         deviceID,
		RegistrationID:   regID,
		IdentityKey:      b64(ikey),
		SignedPrekey:     b64(spk),
		SignedPrekeyID:   spkID,
		SignedPrekeySig:  b64(spkSig),
		AvailablePrekeys: available,
	}
	if pkBytes != nil {
		b.OneTimePrekey = &OneTimePrekeyOut{ID: int(pkID), PublicKey: b64(pkBytes)}
	}
	return b, nil
}
