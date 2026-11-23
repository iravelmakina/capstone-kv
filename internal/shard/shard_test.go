package shard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iravelmakina/capstone-kv/internal/store"
)

func newShard(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(st))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestKeyValueLifecycle(t *testing.T) {
	srv := newShard(t, t.TempDir())
	// Zero bytes and a slash inside the key must survive the trip through the URL.
	url := srv.URL + KeyPath(StorageKey("users", "a/b", ""))

	if code, _ := do(t, "GET", url, ""); code != 404 {
		t.Fatalf("read of missing key = %d, want 404", code)
	}
	if code, _ := do(t, "PUT", url, `{"name":"Ira"}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if code, body := do(t, "GET", url, ""); code != 200 || body != `{"name":"Ira"}` {
		t.Fatalf("read = %d %q", code, body)
	}
	if code, body := do(t, "HEAD", url, ""); code != 200 || body != "" {
		t.Fatalf("exists = %d %q, want 200 with no body", code, body)
	}
	if code, _ := do(t, "DELETE", url, ""); code != 204 {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _ := do(t, "DELETE", url, ""); code != 404 {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func TestDataSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	srv := newShard(t, dir)
	url := KeyPath(StorageKey("t", "k", ""))
	do(t, "PUT", srv.URL+url, `{"v":1}`)
	srv.Close()

	srv2 := newShard(t, dir) // same data directory, as after a container restart
	if code, body := do(t, "GET", srv2.URL+url, ""); code != 200 || body != `{"v":1}` {
		t.Fatalf("after restart: %d %q", code, body)
	}
}

// The zero-byte separators keep field boundaries unambiguous, as long as key values
// themselves contain no zero bytes (the coordinator rejects those with 400).
func TestStorageKeysDontCollide(t *testing.T) {
	pairs := [][2]string{
		{StorageKey("ab", "c", ""), StorageKey("a", "bc", "")},
		{StorageKey("t", "ab", "c"), StorageKey("t", "a", "bc")},
		{StorageKey("t", "k", ""), StorageKey("t", "k", "x")},
	}
	for _, p := range pairs {
		if p[0] == p[1] {
			t.Fatalf("collision: %q", p[0])
		}
	}
}
