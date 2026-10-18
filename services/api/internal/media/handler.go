// SPDX-License-Identifier: AGPL-3.0-or-later
package media

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/middleware"
	"github.com/mintzocloud-coder/sutra/services/api/internal/social"
)

// maxUploadBody caps the upload-intent JSON: it is metadata only (kind,
// filename, mime); bytes go straight to SeaweedFS.
const maxUploadBody = 64 << 10

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// targetMediaID parses the {id} path value into a UUID, 400 on garbage.
func targetMediaID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := social.ParseUserID(r.PathValue("id"))
	if err != nil {
		middleware.WriteJSONError(w, r, http.StatusBadRequest,
			"invalid_media_id", "media id must be a UUID")
		return pgtype.UUID{}, false
	}
	return id, true
}

// mediaError maps package errors to envelope responses; unexpected
// errors are logged by the caller and answered 500.
func mediaError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) {
	var client bool
	switch {
	case errors.Is(err, ErrBadKind):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_media_kind", "kind must be 'video' or 'image'")
		client = true
	case errors.Is(err, ErrBadMime):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_mime_type", "mime type must match the media kind (e.g. video/mp4)")
		client = true
	case errors.Is(err, ErrBadFilename):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"invalid_filename", "filename must be 1-255 bytes with no path separators")
		client = true
	case errors.Is(err, ErrMediaNotFound):
		middleware.WriteJSONError(w, r, http.StatusNotFound,
			"media_not_found", "media not found")
		client = true
	case errors.Is(err, ErrMediaNotOwned):
		middleware.WriteJSONError(w, r, http.StatusForbidden,
			"media_not_owned", "this media belongs to another user")
		client = true
	case errors.Is(err, ErrAlreadyCompleted):
		middleware.WriteJSONError(w, r, http.StatusConflict,
			"upload_already_completed", "this upload was already completed")
		client = true
	case errors.Is(err, ErrBytesMissing):
		middleware.WriteJSONError(w, r, http.StatusUnprocessableEntity,
			"upload_bytes_missing", "no bytes found at the upload URL — PUT the file first")
		client = true
	case errors.Is(err, ErrMasterUnreachable):
		w.Header().Set("Retry-After", "5")
		middleware.WriteJSONError(w, r, http.StatusServiceUnavailable,
			"media_unavailable", "media storage temporarily unavailable; please retry")
		client = true
	}
	if client {
		return
	}
	log.Error("media: "+op+" failed", "err", err,
		"request_id", middleware.FromContext(r.Context()))
	middleware.WriteStatusError(w, r, http.StatusInternalServerError, "internal server error")
}

// UploadHandler serves POST /v1/media/upload: mint a media row and hand
// the client a direct SeaweedFS PUT URL. Unknown JSON fields are
// rejected (strict API, same rule as posts/profiles).
func UploadHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ownerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}

		var raw map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUploadBody))
		if err := dec.Decode(&raw); err != nil {
			middleware.WriteJSONError(w, r, http.StatusBadRequest,
				"invalid_json", "request body must be a JSON object")
			return
		}
		for key := range raw {
			switch key {
			case "kind", "filename", "mime_type":
			default:
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"unknown_field", "unknown field: "+key)
				return
			}
		}

		var kind, filename, mime string
		if v, present := raw["kind"]; present {
			if err := json.Unmarshal(v, &kind); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field kind must be a string")
				return
			}
		}
		if v, present := raw["filename"]; present {
			if err := json.Unmarshal(v, &filename); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field filename must be a string")
				return
			}
		}
		if v, present := raw["mime_type"]; present {
			if err := json.Unmarshal(v, &mime); err != nil {
				middleware.WriteJSONError(w, r, http.StatusBadRequest,
					"invalid_json", "field mime_type must be a string")
				return
			}
		}

		sess, err := svc.CreateUpload(r.Context(), ownerID, kind, filename, mime)
		if err != nil {
			mediaError(w, r, log, "create upload", err)
			return
		}
		writeJSON(w, http.StatusCreated, sess)
	}
}

// CompleteHandler serves POST /v1/media/{id}/complete: the client
// finished its PUT; the API verifies bytes on SeaweedFS and advances
// the lifecycle (images → ready, videos → uploaded + transcode queue).
func CompleteHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetMediaID(w, r)
		if !ok {
			return
		}
		callerID, ok := social.CallerUUID(w, r, svc.pool, log)
		if !ok {
			return
		}
		m, err := svc.Complete(r.Context(), id, callerID)
		if err != nil {
			mediaError(w, r, log, "complete upload", err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

// MediaHandler serves GET /v1/media/{id}: the media row (status the
// client polls while a video transcodes). Media ids are unguessable
// UUIDs and rows carry no PII, so any authenticated user may read —
// feed rendering needs this in D5.
func MediaHandler(svc *Service, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := targetMediaID(w, r)
		if !ok {
			return
		}
		m, err := svc.Get(r.Context(), id)
		if err != nil {
			mediaError(w, r, log, "get media", err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}
