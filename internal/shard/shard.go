// Package shard is the storage node: it keeps keys in a store and serves them to the
// coordinator over internal endpoints, and it registers itself with the coordinator at
// start and every few seconds after that (ADR-0013).
//
// The shard knows nothing about tables or key shapes: the coordinator sends it one opaque
// storage key per item. Only the coordinator is allowed to reach it (security group,
// ADR-0014).
package shard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

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

// Handler returns the shard's HTTP routes, backed by st.
func Handler(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": "shard", "keys": st.Len()})
	})
	// A "GET" pattern also matches HEAD requests; the handler tells them apart.
	mux.HandleFunc("GET /internal/v1/kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		key, ok := decodeKey(w, r)
		if !ok {
			return
		}
		v, found := st.Get(key)
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK) // EXISTS: status only, no body
			return
		}
		w.Write(v)
	})
	mux.HandleFunc("PUT /internal/v1/kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		key, ok := decodeKey(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxValue))
		if err != nil {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "value larger than 1 MiB")
			return
		}
		if err := st.Put(key, body); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "write failed: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /internal/v1/kv/{key}", func(w http.ResponseWriter, r *http.Request) {
		key, ok := decodeKey(w, r)
		if !ok {
			return
		}
		existed, err := st.Delete(key)
		switch {
		case err != nil:
			httpx.WriteError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		case !existed:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

func decodeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(r.PathValue("key"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "key is not base64url")
		return "", false
	}
	return string(b), true
}

// Registration is what a shard tells the coordinator about itself.
type Registration struct {
	ID      string `json:"id"`      // stable across restarts: decides ring positions
	Address string `json:"address"` // base URL the coordinator calls, e.g. http://10.20.64.12:8080
}

// RegisterLoop announces reg to the coordinator now and then every interval, until ctx is
// cancelled. Failures are logged and retried on the next tick: the coordinator may start
// later than the shard, or restart.
func RegisterLoop(ctx context.Context, coordinatorURL string, reg Registration, interval time.Duration) {
	body, _ := json.Marshal(reg)
	client := &http.Client{Timeout: 2 * time.Second}
	send := func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, coordinatorURL+"/internal/v1/shards", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("register with %s: %v", coordinatorURL, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("register with %s: status %d", coordinatorURL, resp.StatusCode)
		}
	}
	send()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}
