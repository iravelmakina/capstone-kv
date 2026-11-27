package coordinator

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
