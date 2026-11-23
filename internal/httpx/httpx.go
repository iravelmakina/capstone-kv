// Package httpx holds the small JSON helpers the coordinator and the shard share.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON sends v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// WriteError sends {"error": msg} with the given status, the error shape in api/openapi.yaml.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
