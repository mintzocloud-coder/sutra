// SPDX-License-Identifier: AGPL-3.0-or-later
package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxKeyBody caps key-bundle JSON: a 100-prekey registration is ~6KB
// of base64; 64KB leaves wide headroom while bounding parse cost.
const maxKeyBody = 64 << 10

// prekeyBody is one one-time prekey in a request body.
type prekeyBody struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
}

// registerBody is POST /v1/e2e/devices. All key material is standard
// base64 (with padding); lengths and the signed-prekey signature are
// verified server-side by ValidateBundle.
type registerBody struct {
	DeviceID        string       `json:"device_id"`
	RegistrationID  int          `json:"registration_id"`
	IdentityKey     string       `json:"identity_key"`
	SignedPrekey    string       `json:"signed_prekey"`
	SignedPrekeyID  int          `json:"signed_prekey_id"`
	SignedPrekeySig string       `json:"signed_prekey_sig"`
	OneTimePrekeys  []prekeyBody `json:"one_time_prekeys"`
}

// replenishBody is POST /v1/e2e/devices/{device_id}/prekeys.
type replenishBody struct {
	OneTimePrekeys []prekeyBody `json:"one_time_prekeys"`
}

// decodeBody reads the request body (bounded) and decodes it into v
// with unknown fields rejected — a strict API, same rule as
// media/posts (the D2 DisallowUnknownFields fix).
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKeyBody))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_json", "request body unreadable or exceeds 64KB")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var ute *json.UnmarshalTypeError
		code, msg := "invalid_json", "request body must be a JSON object of the documented shape"
		if ute == nil && strings.Contains(err.Error(), "unknown field") {
			code, msg = "unknown_field", err.Error()
		} else if errors.As(err, &ute) {
			msg = "field " + ute.Field + " has the wrong type"
		}
		middleware.WriteJSONError(w, r, http.StatusBadRequest, code, msg)
		return false
	}
	return true
}

// decodeKey decodes one base64 key field; unparseable input is a 422
// (the shape was valid JSON, the key bytes are not).
func decodeKey(field, s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, &keyEncodingError{field: field}
	}
	return b, nil
}

// keyEncodingError reports WHICH key field failed base64 decoding so
// the client can pinpoint the bad field without guessing.
type keyEncodingError struct{ field string }

func (e *keyEncodingError) Error() string {
	return "e2e: field " + e.field + " is not valid standard base64"
}

// toBundle converts a decoded registerBody into the domain KeyBundle,
// decoding every base64 field.
func toBundle(in registerBody) (KeyBundle, error) {
	var b KeyBundle
	var err error
	b.DeviceID = in.DeviceID
	b.RegistrationID = in.RegistrationID
	b.SignedPrekeyID = in.SignedPrekeyID
	if b.IdentityKey, err = decodeKey("identity_key", in.IdentityKey); err != nil {
		return b, err
	}
	if b.SignedPrekey, err = decodeKey("signed_prekey", in.SignedPrekey); err != nil {
		return b, err
	}
	if b.SignedPrekeySig, err = decodeKey("signed_prekey_sig", in.SignedPrekeySig); err != nil {
		return b, err
	}
	b.OneTimePrekeys = make([]OneTimePrekey, len(in.OneTimePrekeys))
	for i, k := range in.OneTimePrekeys {
		if b.OneTimePrekeys[i].PublicKey, err = decodeKey("one_time_prekeys.public_key", k.PublicKey); err != nil {
			return b, err
		}
		b.OneTimePrekeys[i].ID = k.ID
	}
	return b, nil
}

// toPrekeys converts a decoded replenishBody into domain prekeys.
func toPrekeys(in replenishBody) ([]OneTimePrekey, error) {
	out := make([]OneTimePrekey, len(in.OneTimePrekeys))
	for i, k := range in.OneTimePrekeys {
		pk, err := decodeKey("one_time_prekeys.public_key", k.PublicKey)
		if err != nil {
			return nil, err
		}
		out[i] = OneTimePrekey{ID: k.ID, PublicKey: pk}
	}
	return out, nil
}

// e2eError maps package errors to envelope responses; unexpected
// errors are logged by the caller and answered 500.
func e2eError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	var client bool
	switch {
	case errors.Is(err, ErrDeviceNotFound):
		middleware.WriteJSONError(w, r, http.StatusNotFound,
			"e2e_device_not_found", "no such device for this user")
		client = true
	case errors.Is(err, ErrPrekeyIDConflict):
		middleware.WriteJSONError(w, r, http.StatusConflict,
			"e2e_prekey_id_conflict", err.Error())
		client = true
	case errors.Is(err, ErrBadSignature), errors.Is(err, ErrInvalidBundle):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"e2e_invalid_bundle", err.Error())
		client = true
	default:
		var kee *keyEncodingError
		if errors.As(err, &kee) {
			middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
				"e2e_invalid_key_encoding", kee.Error())
			client = true
		}
	}
	if client {
		return
	}
	log.Error("e2e: "+op+" failed", "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// targetUserID parses the {id} path value (the directory target) into
// a UUID, 400 on garbage.
func targetUserID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := social.ParseUserID(r.PathValue("id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_user_id", "user id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// DevicesHandler serves /v1/e2e/devices, dispatching on method like
// FollowHandler does: POST registers (or re-registers) the caller's
// device, GET lists the caller's devices. Anything else → 405 + Allow.
func DevicesHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	register := RegisterHandler(svc, log)
	list := ListHandler(svc, log)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			register(w, r)
		case http.MethodGet:
			list(w, r)
		default:
			middleware.MethodNotAllowedHandler(http.MethodPost, http.MethodGet)(w, r)
		}
	}
}

// RegisterHandler serves POST /v1/e2e/devices: verify + persist the
// caller's device key bundle. 201 on first registration, 200 when the
// device re-registers (key material replaced). The signed-prekey
// signature is verified against the identity key before anything is
// stored — a bundle that fails verification is rejected, never
// persisted.
func RegisterHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		var in registerBody
		if !decodeBody(w, r, &in) {
			return
		}
		bundle, err := toBundle(in)
		if err != nil {
			e2eError(w, r, log, "decode bundle", err)
			return
		}
		dev, created, err := svc.RegisterDevice(r.Context(), callerID, bundle)
		if err != nil {
			e2eError(w, r, log, "register device", err)
			return
		}
		if created {
			writeJSON(w, http.StatusCreated, dev)
			return
		}
		writeJSON(w, http.StatusOK, dev)
	}
}

// ListHandler serves GET /v1/e2e/devices: the caller's devices (ids,
// counts, timestamps — no key material).
func ListHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		devs, err := svc.Devices(r.Context(), callerID)
		if err != nil {
			e2eError(w, r, log, "list devices", err)
			return
		}
		writeJSON(w, http.StatusOK, devs)
	}
}

// ReplenishHandler serves POST /v1/e2e/devices/{device_id}/prekeys:
// add one-time prekeys to the caller's device queue. Idempotent on
// retry (same id + same key = no-op); conflicting bytes on an existing
// id is a 409.
func ReplenishHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		deviceID := r.PathValue("device_id")
		if deviceID == "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_device_id", "device id must not be empty")
			return
		}
		var in replenishBody
		if !decodeBody(w, r, &in) {
			return
		}
		keys, err := toPrekeys(in)
		if err != nil {
			e2eError(w, r, log, "decode prekeys", err)
			return
		}
		available, err := svc.ReplenishPrekeys(r.Context(), callerID, deviceID, keys)
		if err != nil {
			e2eError(w, r, log, "replenish prekeys", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"device_id":         deviceID,
			"available_prekeys": available,
		})
	}
}

// DeleteHandler serves DELETE /v1/e2e/devices/{device_id}: remove the
// caller's device (the one-time queue cascades). The device's private
// keys die with it — there is no server-side recovery, by design.
func DeleteHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		deviceID := r.PathValue("device_id")
		if deviceID == "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_device_id", "device id must not be empty")
			return
		}
		if err := svc.DeleteDevice(r.Context(), callerID, deviceID); err != nil {
			e2eError(w, r, log, "delete device", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// DirectoryHandler serves GET /v1/users/{id}/devices: the public key
// directory for any user. Any authenticated user may read it —
// initiating an E2E session requires the peer's public bundle.
func DirectoryHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := targetUserID(w, r)
		if !ok {
			return
		}
		devs, err := svc.Directory(r.Context(), target)
		if err != nil {
			e2eError(w, r, log, "device directory", err)
			return
		}
		writeJSON(w, http.StatusOK, devs)
	}
}

// PrekeyHandler serves GET
// /v1/users/{id}/devices/{device_id}/prekey-bundle: fetch the device's
// public bundle and atomically consume one one-time prekey for X3DH
// session initiation. Each prekey is handed out at most once; when the
// queue is dry the bundle still returns (one_time_prekey: null) and
// the initiator falls back to signed-prekey-only X3DH.
func PrekeyHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := targetUserID(w, r)
		if !ok {
			return
		}
		deviceID := r.PathValue("device_id")
		if deviceID == "" {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_device_id", "device id must not be empty")
			return
		}
		b, err := svc.ConsumePrekey(r.Context(), target, deviceID)
		if err != nil {
			e2eError(w, r, log, "consume prekey", err)
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}
