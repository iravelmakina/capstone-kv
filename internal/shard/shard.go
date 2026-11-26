// Package shard is the storage node: one replica of a shard's replica set (ADR-0020). It
// keeps keys in a store, serves them to the coordinator over internal endpoints, ships its
// log to followers when it leads, and reports to the coordinator every few seconds.
//
// The shard knows nothing about tables or key shapes: the coordinator sends it one opaque
// storage key per item. Only the coordinator and the other replicas may reach it (security
// group, ADR-0020).
package shard

import (
	"encoding/base64"
	"net/http"

	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// maxValue limits one stored value; larger bodies get 413.
const maxValue = 1 << 20 // 1 MiB

// StorageKey is the key a shard stores an item under: table, partition key and sort key
// joined by zero bytes. The sort key is empty for tables without one.
func StorageKey(table, pk, sk string) string {
	return table + "\x00" + pk + "\x00" + sk
}

// KeyPath is the internal URL path for a storage key. The key is base64url-encoded so
// that zero bytes and "/" in key values can't break the path.
func KeyPath(storageKey string) string {
	return "/internal/v1/kv/" + base64.RawURLEncoding.EncodeToString([]byte(storageKey))
}

// Handler returns the routes of a replica set of one, backed by st: the M2 shard, kept
// for running without replication (and for the coordinator's tests until M3's group 4).
func Handler(st *store.Store) http.Handler {
	return NewStandalone(st).Handler()
}

func decodeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(r.PathValue("key"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "key is not base64url")
		return "", false
	}
	return string(b), true
}
