package coordinator

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// cluster is a coordinator and n shards running in-process, wired like the deployment.
type cluster struct {
	coord   *httptest.Server
	members *Membership
	shards  map[string]*httptest.Server // shard ID -> server
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newCluster starts a coordinator expecting n shards and, if register is true, n shards
// that have registered once.
func newCluster(t *testing.T, n int, register bool) *cluster {
	t.Helper()
	members := NewMembership(ring.New(ring.DefaultVirtualNodes), 15*time.Second, n)
	coord := httptest.NewServer(New(openStore(t), members).Handler())
	t.Cleanup(coord.Close)
	c := &cluster{coord: coord, members: members, shards: map[string]*httptest.Server{}}
	if register {
		for i := 1; i <= n; i++ {
			c.addShard(t, fmt.Sprintf("shard-%d", i))
		}
	}
	return c
}

func (c *cluster) addShard(t *testing.T, id string) {
	t.Helper()
	srv := httptest.NewServer(shard.Handler(openStore(t)))
	t.Cleanup(srv.Close)
	c.shards[id] = srv
	code, _ := call(t, "POST", c.coord.URL+"/internal/v1/shards", fmt.Sprintf(`{"id":%q,"address":%q}`, id, srv.URL))
	if code != 200 {
		t.Fatalf("register %s = %d", id, code)
	}
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func expect(t *testing.T, gotCode int, gotBody string, wantCode int) {
	t.Helper()
	if gotCode != wantCode {
		t.Fatalf("status %d, want %d (body %q)", gotCode, wantCode, gotBody)
	}
}

func TestTableRegistration(t *testing.T) {
	c := newCluster(t, 2, true)
	code, body := call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	expect(t, code, body, 201)
	code, body = call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"other"}`)
	expect(t, code, body, 409) // spec "Duplicate table"
	code, body = call(t, "GET", c.coord.URL+"/v1/tables/users", "")
	if code != 200 || !strings.Contains(body, `"partitionKey":"userId"`) {
		t.Fatalf("definition changed or missing: %d %s", code, body)
	}
	code, body = call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"bad name","partitionKey":"k"}`)
	expect(t, code, body, 400)
	code, body = call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"t","partitionKey":"k","extra":1}`)
	expect(t, code, body, 400)
}

func TestItemLifecycle(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	url := c.coord.URL + "/v1/tables/users/items/u1"

	code, body := call(t, "PUT", url, `{ "name": "Ira" }`)
	expect(t, code, body, 200)
	code, body = call(t, "GET", url, "")
	if code != 200 || body != `{"name":"Ira"}` {
		t.Fatalf("read = %d %q", code, body) // spec "Put then read" (stored compacted)
	}
	code, body = call(t, "HEAD", url, "")
	if code != 200 || body != "" {
		t.Fatalf("exists = %d %q", code, body) // spec "Exists"
	}
	code, _ = call(t, "HEAD", c.coord.URL+"/v1/tables/users/items/never", "")
	expect(t, code, "", 404)
	code, body = call(t, "DELETE", url, "")
	expect(t, code, body, 204) // spec "Delete"
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 404)
}

func TestBadRequests(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"orders","partitionKey":"customerId","sortKey":"orderId"}`)

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"unknown table", "GET", "/v1/tables/nope/items/k", "", 404},
		{"sort key missing", "GET", "/v1/tables/orders/items/c1", "", 400},
		{"sort key not allowed", "GET", "/v1/tables/users/items/u1/x", "", 400},
		{"body not an object", "PUT", "/v1/tables/users/items/u1", `[1,2]`, 400},
		{"body not JSON", "PUT", "/v1/tables/users/items/u1", `hello`, 400},
		{"zero byte in key", "GET", "/v1/tables/users/items/a%00b", "", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := call(t, tc.method, c.coord.URL+tc.path, tc.body)
			expect(t, code, body, tc.want)
		})
	}
}

// Spec "Same partition, same shard": items differing only in sort key share a shard.
func TestSortKeyDoesNotChangeShard(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"orders","partitionKey":"customerId","sortKey":"orderId"}`)
	for i := 0; i < 20; i++ {
		call(t, "PUT", fmt.Sprintf("%s/v1/tables/orders/items/c7/o%d", c.coord.URL, i), `{"n":1}`)
	}
	holding := 0
	for _, srv := range c.shards {
		_, body := call(t, "GET", srv.URL+"/health", "")
		if !strings.Contains(body, `"keys":0`) {
			holding++
		}
	}
	if holding != 1 {
		t.Fatalf("items of one partition are on %d shards, want 1", holding)
	}
}

// Keys spread over both shards (sanity check that routing really uses the ring).
func TestKeysSpreadOverShards(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	for i := 0; i < 200; i++ {
		call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), `{"n":1}`)
	}
	for id, srv := range c.shards {
		if _, body := call(t, "GET", srv.URL+"/health", ""); strings.Contains(body, `"keys":0`) {
			t.Fatalf("%s holds no keys", id)
		}
	}
}

// Spec "Coordinator restart": item requests get 503 until the expected shards registered.
func TestNotReadyUntilShardsRegister(t *testing.T) {
	c := newCluster(t, 2, false)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`) // works without shards
	url := c.coord.URL + "/v1/tables/users/items/u1"

	code, body := call(t, "GET", url, "")
	expect(t, code, body, 503)
	c.addShard(t, "shard-1")
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 503) // one of two
	c.addShard(t, "shard-2")
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 404) // ready: the item just doesn't exist
}

// Spec "Shard down": a silent shard stays on the ring; its keys get 503, others still work.
func TestSilentShardAnswers503OnlyForItsKeys(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)

	// Let time pass: shard-1 keeps sending heartbeats, shard-2 has gone silent.
	later := time.Now().Add(20 * time.Second)
	c.members.now = func() time.Time { return later }
	c.members.Register("shard-1", c.shards["shard-1"].URL)

	var ok, unavailable int
	for i := 0; i < 50; i++ {
		code, _ := call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), `{"n":1}`)
		switch code {
		case 200:
			ok++
		case 503:
			unavailable++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok == 0 || unavailable == 0 {
		t.Fatalf("ok=%d unavailable=%d: want both, keys of the live shard work and of the silent one get 503", ok, unavailable)
	}
	_, list := call(t, "GET", c.coord.URL+"/internal/v1/shards", "")
	if !strings.Contains(list, `"id":"shard-2","address"`) || !strings.Contains(list, `"available":false`) {
		t.Fatalf("silent shard should be listed as unavailable: %s", list)
	}
}

// A shard that is alive by heartbeat but doesn't answer also yields 503, not 404.
func TestUnreachableShardIs503(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	c.shards["shard-2"].Close()
	saw503 := false
	for i := 0; i < 50 && !saw503; i++ {
		code, _ := call(t, "GET", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), "")
		if code != 404 && code != 503 {
			t.Fatalf("status %d", code)
		}
		saw503 = code == 503
	}
	if !saw503 {
		t.Fatal("no key reached the closed shard")
	}
}

func TestExplicitRemove(t *testing.T) {
	c := newCluster(t, 2, true)
	code, body := call(t, "DELETE", c.coord.URL+"/internal/v1/shards/shard-2", "")
	expect(t, code, body, 204)
	code, body = call(t, "DELETE", c.coord.URL+"/internal/v1/shards/shard-2", "")
	expect(t, code, body, 404)
}

func TestHealth(t *testing.T) {
	c := newCluster(t, 2, true)
	code, body := call(t, "GET", c.coord.URL+"/health", "")
	if code != 200 || !strings.Contains(body, `"role":"coordinator"`) || !strings.Contains(body, `"ready":true`) ||
		!strings.Contains(body, `"version":"dev"`) { // tests run unlinked builds: version "dev"
		t.Fatalf("health = %d %s", code, body)
	}
}

// Spec "Coordinator not ready": 503 with the same body, ready false.
func TestHealthNotReady(t *testing.T) {
	c := newCluster(t, 2, false)
	code, body := call(t, "GET", c.coord.URL+"/health", "")
	if code != 503 || !strings.Contains(body, `"ready":false`) || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("health = %d %s; want 503 with ready false", code, body)
	}
}
