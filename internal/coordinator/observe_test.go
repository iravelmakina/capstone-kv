package coordinator

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iravelmakina/capstone-kv/internal/auth"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

func scrapeCoordinator(c *cluster) string {
	rec := httptest.NewRecorder()
	c.obj.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// Spec "Request counted": a request is counted by operation, code, shard and principal,
// timed per shard, and answered with its request ID.
func TestRequestCountedAndID(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	req, _ := http.NewRequest("PUT", c.coord.URL+"/v1/tables/users/items/u1", bytes.NewReader([]byte(`{"a":1}`)))
	req.Header.Set("X-Request-ID", "req-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != "req-42" {
		t.Fatalf("request ID answered %q, want req-42", got)
	}
	code, _ := call(t, "GET", c.coord.URL+"/v1/tables/users/items/u1", "")
	expect(t, code, "", 200)

	m := scrapeCoordinator(c)
	for _, want := range []string{
		`kv_requests_total{op="write",code="200",shard="shard-1",principal="-"} 1`,
		`kv_requests_total{op="read",code="200",shard="shard-1",principal="-"} 1`,
		`kv_requests_total{op="table",code="201",shard="",principal="-"} 1`,
		`kv_request_duration_seconds_count{op="write",shard="shard-1"} 1`,
		`kv_replica_up{shard="shard-1",replica="1a",node="127.0.0.1"} 1`,
		`kv_shard_has_leader{shard="shard-1"} 1`,
		`kv_shard_leader_epoch{shard="shard-1",leader="1a"} 1`,
		"go_goroutines ",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics miss %q:\n%s", want, m)
		}
	}
}

// A request without an ID gets one, and replicas receive the same ID the client gets.
func TestRequestIDPassedToReplicas(t *testing.T) {
	fakes := [3]*fakeReplica{
		{status: 200, body: `{"a":1}`, version: store.Version{Epoch: 1, LSN: 5}},
		{status: 200, body: `{"a":1}`, version: store.Version{Epoch: 1, LSN: 5}},
		{status: 200, body: `{"a":1}`, version: store.Version{Epoch: 1, LSN: 5}},
	}
	url := fakeShard(t, fakes)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := resp.Header.Get("X-Request-ID")
	if len(id) != 16 {
		t.Fatalf("made-up request ID %q, want 16 hex characters", id)
	}
	seen := 0
	for _, f := range fakes {
		if got, _ := f.lastID.Load().(string); got == id {
			seen++
		}
	}
	if seen != 2 { // R = 2 replicas asked
		t.Fatalf("%d replicas got request ID %s, want 2", seen, id)
	}
}

// keyed gives the cluster's coordinator a keys file with an admin key ("ops") and a
// read+write key ("mcp"), and returns a request helper that sends a key.
func keyed(t *testing.T, c *cluster) func(method, url, body, key string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	os.WriteFile(path, []byte(`[
		{"name":"ops","sha256":"`+auth.Hash("ops-key")+`","scopes":["read","write","delete","admin"]},
		{"name":"mcp","sha256":"`+auth.Hash("mcp-key")+`","scopes":["read","write"]}]`), 0o600)
	keys, err := auth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.obj.UseKeys(keys)
	return func(method, url, body, key string) (int, string) {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}

// Specs "No key", "Missing scope" and "Who deleted it".
func TestAPIKeys(t *testing.T) {
	c := newCluster(t, 1, true)
	do := keyed(t, c)
	items := c.coord.URL + "/v1/tables/users/items/u1"
	if code, _ := do("POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`, "mcp-key"); code != 403 {
		t.Fatalf("create table with a read+write key = %d, want 403", code)
	}
	if code, _ := do("POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`, "ops-key"); code != 201 {
		t.Fatalf("create table with the admin key = %d, want 201", code)
	}
	if code, _ := do("PUT", items, `{"a":1}`, "mcp-key"); code != 200 {
		t.Fatalf("put with a write key = %d", code)
	}
	if code, _ := do("GET", items, "", ""); code != 401 {
		t.Fatalf("read without a key = %d, want 401", code)
	}
	if code, _ := do("GET", items, "", "wrong"); code != 401 {
		t.Fatalf("read with an unknown key = %d, want 401", code)
	}
	if code, _ := do("DELETE", items, "", "mcp-key"); code != 403 {
		t.Fatalf("delete without the delete scope = %d, want 403", code)
	}
	if code, _ := do("GET", items, "", "mcp-key"); code != 200 {
		t.Fatalf("item gone after a refused delete (GET = %d)", code)
	}
	if code, _ := do("DELETE", items, "", "ops-key"); code != 204 {
		t.Fatalf("delete with the delete scope = %d", code)
	}
	if code, _ := call(t, "GET", c.coord.URL+"/health", ""); code != 200 {
		t.Fatalf("/health needs no key, got %d", code)
	}
	m := scrapeCoordinator(c)
	for _, want := range []string{
		`kv_requests_total{op="write",code="200",shard="shard-1",principal="mcp"} 1`,
		`kv_requests_total{op="delete",code="403",shard="",principal="mcp"} 1`,
		`kv_requests_total{op="delete",code="204",shard="shard-1",principal="ops"} 1`,
		`kv_requests_total{op="read",code="401",shard="",principal="-"} 2`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics miss %q", want)
		}
	}
}
