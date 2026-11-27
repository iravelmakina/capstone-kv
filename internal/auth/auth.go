// Package auth checks API keys on the public API (ADR-0027). Keys are stored only as
// SHA-256 hashes, with a name and scopes, in a JSON file that the coordinator reloads when
// it changes, so a key is added or revoked without a restart:
//
//	[{"name": "ops", "sha256": "<hex>", "scopes": ["read", "write", "delete", "admin"]}, ...]
//
// The file comes from a Secrets Manager secret in AWS and from `make keys` locally; key
// values themselves are never in the repository or in a log line.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scopes, one per kind of operation.
const (
	Read   = "read"   // GET and HEAD on items; list and describe tables
	Write  = "write"  // PUT on items
	Delete = "delete" // DELETE on items
	Admin  = "admin"  // create tables
)

// Key is one API key as stored: its name, the hash of its value, and what it may do.
type Key struct {
	Name   string   `json:"name"`
	SHA256 string   `json:"sha256"`
	Scopes []string `json:"scopes"`
}

// Allows reports whether the key has the scope.
func (k Key) Allows(scope string) bool { return slices.Contains(k.Scopes, scope) }

// Hash returns the hex SHA-256 of a key value, the form the keys file holds.
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Keyring is the set of valid keys, reloaded from its file.
type Keyring struct {
	path    string
	mu      sync.RWMutex
	byHash  map[string]Key
	modTime time.Time
	size    int64
}

// Load reads the keys file. A configured file that is missing or invalid is an error: the
// coordinator then refuses to start rather than serve without keys.
func Load(path string) (*Keyring, error) {
	k := &Keyring{path: path}
	if err := k.reload(); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *Keyring) reload() error {
	info, err := os.Stat(k.path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(k.path)
	if err != nil {
		return err
	}
	var keys []Key
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("%s: %w", k.path, err)
	}
	byHash := make(map[string]Key, len(keys))
	for _, key := range keys {
		h := strings.ToLower(key.SHA256)
		if len(h) != 64 || key.Name == "" {
			return fmt.Errorf("%s: every key needs a name and a 64-character sha256", k.path)
		}
		byHash[h] = key
	}
	k.mu.Lock()
	k.byHash, k.modTime, k.size = byHash, info.ModTime(), info.Size()
	k.mu.Unlock()
	return nil
}

// Watch reloads the file whenever its time or size changes, checking every interval,
// until ctx ends. A file that becomes invalid is reported and the previous keys stay.
func (k *Keyring) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			info, err := os.Stat(k.path)
			if err != nil {
				slog.Error("api keys file unreadable; keeping the loaded keys", "path", k.path, "error", err)
				continue
			}
			k.mu.RLock()
			same := info.ModTime().Equal(k.modTime) && info.Size() == k.size
			k.mu.RUnlock()
			if same {
				continue
			}
			if err := k.reload(); err != nil {
				slog.Error("api keys file invalid; keeping the loaded keys", "error", err)
				continue
			}
			k.mu.RLock()
			n := len(k.byHash)
			k.mu.RUnlock()
			slog.Info("api keys reloaded", "keys", n)
		}
	}
}

// Check returns the key whose value is token.
func (k *Keyring) Check(token string) (Key, bool) {
	if token == "" {
		return Key{}, false
	}
	h := Hash(token)
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.byHash[h]
	return key, ok
}

// Bearer returns the token from an "Authorization: Bearer <token>" header value.
func Bearer(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
