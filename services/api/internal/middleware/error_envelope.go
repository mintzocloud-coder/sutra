// SPDX-License-Identifier: AGPL-3.0-or-later
// Shared JSON error envelope writer for the Sutra API service.
//
// The canonical shape is {"error":{"code","message","request_id"}}.
// (A6 will consolidate every error path onto this helper; it is introduced
// here so the rate limiter and later middleware don't each invent their
// own format.)
package middleware

import (
	"encoding/json"
	"net/http"
)

// WriteJSONError sends status with a JSON error envelope containing a
// machine-readable code, a human-readable message, and the request ID so
// clients and operators can correlate with access logs.
func WriteJSONError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":       code,
			"message":    message,
			"request_id": FromContext(r.Context()),
		},
	})
	_, _ = w.Write(body)
}
