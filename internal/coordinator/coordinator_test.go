package coordinator

import (
	"context"
	"encoding/json"
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

// node is one shard replica running in-process.
type node struct {
	shard, id string
	r         *shard.Replica
	srv       *httptest.Server
	cancel    context.CancelFunc
}

// cluster is a coordinator and n shards of 3 replicas each, running in-process and wired
// like the deployment. Heartbeats are sent by beat(), not by timers, so tests control them.
type cluster struct {
	coord   *httptest.Server
	members *Membership
	leaders *store.Store
	nodes   map[string]*node // replica ID ("1a", "2c", ...) -> node
	shards  int
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

// newCluster starts a coordinator expecting n shards and their 3n replicas; if register
// is true, every replica has reported in and knows its role.
func newCluster(t *testing.T, n int, register bool) *cluster {
	t.Helper()
	c := &cluster{nodes: map[string]*node{}, shards: n, leaders: openStore(t)}
	c.startCoordinator(t)
	for s := 1; s <= n; s++ {
		for _, letter := range []string{"a", "b", "c"} {
			c.startNode(t, fmt.Sprintf("shard-%d", s), fmt.Sprintf("%d%s", s, letter))
		}
	}
	if register {
		c.beat(t)
	}
	return c
}

func (c *cluster) startCoordinator(t *testing.T) {
	t.Helper()
	members, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: c.shards, Replicas: 3, Leaders: c.leaders,
		AskTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.members = members
	c.coord = httptest.NewServer(New(openStore(t), members, 2).Handler())
	t.Cleanup(c.coord.Close)
}

func (c *cluster) startNode(t *testing.T, shardID, id string) *node {
	t.Helper()
	r := shard.NewReplica(openStore(t), shard.Config{Shard: shardID, Replica: id, W: 2,
		CommitTimeout: time.Second, LongPoll: 200 * time.Millisecond, PullDeadline: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{shard: shardID, id: id, r: r, srv: httptest.NewServer(r.Handler()), cancel: cancel}
	go r.Run(ctx)
	t.Cleanup(n.stop)
	c.nodes[id] = n
	return n
}

func (n *node) stop() {
	n.cancel()
	n.srv.CloseClientConnections()
	n.srv.Close()
}

// beat sends one heartbeat for each named replica (all if none) and applies the answer,
// twice, so that replicas that reported before their set had a leader learn it too.
func (c *cluster) beat(t *testing.T, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		for id := range c.nodes {
			ids = append(ids, id)
		}
	}
	for round := 0; round < 2; round++ {
		for _, id := range ids {
			n := c.nodes[id]
			s := n.r.Status()
			hb, _ := json.Marshal(shard.Heartbeat{Shard: n.shard, Replica: id, Address: n.srv.URL, Epoch: s.Epoch, Last: s.Last})
			code, body := call(t, "POST", c.coord.URL+"/internal/v1/replicas", string(hb))
			if code != 200 {
				t.Fatalf("heartbeat %s = %d %s", id, code, body)
			}
			var a shard.HeartbeatAnswer
			json.Unmarshal([]byte(body), &a)
			n.r.SetRole(a.Epoch, a.Leader, a.LeaderAddress)
		}
	}
}

// others returns every replica of the cluster except the given one.
func (c *cluster) others(except string) []string {
	var ids []string
	for id := range c.nodes {
		if id != except {
			ids = append(ids, id)
		}
	}
	return ids
}

// leaderOf returns the node leading a shard.
func (c *cluster) leaderOf(t *testing.T, shardID string) *node {
	t.Helper()
	for _, s := range c.members.Status() {
		if s.ID == shardID {
			return c.nodes[s.Leader]
		}
	}
	t.Fatalf("no leader for %s", shardID)
	return nil
}

// later moves the coordinator's clock forward, as if time passed without heartbeats.
func (c *cluster) later(d time.Duration) {
	at := time.Now().Add(d)
	c.members.mu.Lock()
	c.members.now = func() time.Time { return at }
	c.members.mu.Unlock()
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
	for _, s := range []string{"shard-1", "shard-2"} {
		if c.leaderOf(t, s).r.Status().Keys > 0 {
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
	for _, s := range []string{"shard-1", "shard-2"} {
		if c.leaderOf(t, s).r.Status().Keys == 0 {
			t.Fatalf("%s holds no keys", s)
		}
	}
}

// Spec "Coordinator restart" (M2) and "One replica missing at start": item requests get
// 503 until every expected shard has a leader, which needs 2 of its 3 replicas.
func TestNotReadyUntilShardsRegister(t *testing.T) {
	c := newCluster(t, 2, false)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`) // works without shards
	url := c.coord.URL + "/v1/tables/users/items/u1"

	code, body := call(t, "GET", url, "")
	expect(t, code, body, 503)
	c.beat(t, "1a", "1b", "1c")
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 503) // one of two shards
	c.beat(t, "2a")            // one replica isn't a majority: no leader yet
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 503)
	c.beat(t, "2a", "2b") // 2c stays down
	code, body = call(t, "GET", url, "")
	expect(t, code, body, 404) // ready: the item just doesn't exist
	for i := 0; i < 20; i++ {  // keys of both shards can be written with 2c missing
		code, body = call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), `{"v":1}`)
		expect(t, code, body, 200)
	}
}

// Spec "Leader down": its shard's writes get 503, reads still work from the followers.
func TestLeaderDownWritesFailReadsWork(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	url := c.coord.URL + "/v1/tables/users/items/u1"
	code, body := call(t, "PUT", url, `{"v":1}`)
	expect(t, code, body, 200)

	leader := c.leaderOf(t, "shard-1")
	leader.stop()
	c.later(20 * time.Second)
	c.beat(t, c.others(leader.id)...)

	code, body = call(t, "PUT", url, `{"v":2}`)
	expect(t, code, body, 503)
	code, body = call(t, "GET", url, "")
	if code != 200 || body != `{"v":1}` {
		t.Fatalf("read with the leader down = %d %q", code, body)
	}
	if s := c.members.Status()[0]; s.Unavailable != 1 {
		t.Fatalf("503 count = %d", s.Unavailable)
	}
}

// A leader that is alive by heartbeat but doesn't answer: writes 503, reads still work.
func TestUnreachableLeader(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	url := c.coord.URL + "/v1/tables/users/items/u1"
	code, body := call(t, "PUT", url, `{"v":1}`)
	expect(t, code, body, 200)
	c.leaderOf(t, "shard-1").stop()
	code, body = call(t, "PUT", url, `{"v":2}`)
	expect(t, code, body, 503)
	for i := 0; i < 6; i++ { // every rotation, also those that start at the dead leader
		if code, body := call(t, "GET", url, ""); code != 200 {
			t.Fatalf("read %d = %d %s", i, code, body)
		}
	}
}

// Spec "Read after write": a committed put is read back with any one replica stopped.
func TestReadAfterWriteWithAnyReplicaDown(t *testing.T) {
	for _, down := range []string{"1a", "1b", "1c"} {
		t.Run(down, func(t *testing.T) {
			c := newCluster(t, 1, true)
			call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
			url := c.coord.URL + "/v1/tables/users/items/u1"
			call(t, "PUT", url, `{"v":1}`)
			code, body := call(t, "PUT", url, `{"v":2}`)
			expect(t, code, body, 200)
			c.nodes[down].stop()
			for i := 0; i < 6; i++ {
				if code, body := call(t, "GET", url, ""); code != 200 || body != `{"v":2}` {
					t.Fatalf("read %d with %s down = %d %q", i, down, code, body)
				}
			}
		})
	}
}

// Spec "Both followers down": the write is answered 504 and counted.
func TestBothFollowersDownIs504(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	leader := c.leaderOf(t, "shard-1")
	for _, id := range c.others(leader.id) {
		c.nodes[id].stop()
	}
	code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/u1", `{"v":1}`)
	expect(t, code, body, 504)
	if s := c.members.Status()[0]; s.NotCommitted != 1 {
		t.Fatalf("504 count = %d", s.NotCommitted)
	}
	if _, health := call(t, "GET", c.coord.URL+"/health", ""); !strings.Contains(health, `"notCommitted504":1`) {
		t.Fatalf("504 count missing from health: %s", health)
	}
}

// Spec "Two replicas down": reads 503.
func TestTwoReplicasDownReadsFail(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	c.nodes["1b"].stop()
	c.nodes["1c"].stop()
	code, body := call(t, "GET", c.coord.URL+"/v1/tables/users/items/u1", "")
	expect(t, code, body, 503)
}

// Spec "Leader replaced": with both followers live, the newer one becomes leader under a
// higher epoch, and committed writes survive.
func TestAutomaticPromotion(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	for i := 0; i < 10; i++ {
		code, body := call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), `{"v":1}`)
		expect(t, code, body, 200)
	}
	old := c.leaderOf(t, "shard-1")
	old.stop()
	c.later(20 * time.Second)
	followers := c.others(old.id)
	c.beat(t, followers...)
	c.members.CheckLeaders(context.Background())
	c.beat(t, followers...) // they learn the new epoch

	s := c.members.Status()[0]
	if s.Epoch != 2 || s.Leader == old.id {
		t.Fatalf("after promotion: epoch %d leader %s", s.Epoch, s.Leader)
	}
	code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/new", `{"v":2}`)
	expect(t, code, body, 200)
	for i := 0; i < 10; i++ {
		code, body := call(t, "GET", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), "")
		if code != 200 {
			t.Fatalf("committed u%d lost after promotion: %d %s", i, code, body)
		}
	}
}

// Spec "Only one follower live": no promotion, writes 503.
func TestNoPromotionWithOneFollower(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	old := c.leaderOf(t, "shard-1")
	old.stop()
	followers := c.others(old.id)
	c.nodes[followers[1]].stop()
	c.later(20 * time.Second)
	c.beat(t, followers[0])
	c.members.CheckLeaders(context.Background())
	if s := c.members.Status()[0]; s.Epoch != 1 || s.Leader != old.id {
		t.Fatalf("promoted with one follower: epoch %d leader %s", s.Epoch, s.Leader)
	}
	code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/u1", `{"v":1}`)
	expect(t, code, body, 503)
}

// Spec "Coordinator restart keeps the leader": the assignment is read back from disk.
func TestCoordinatorRestartKeepsLeader(t *testing.T) {
	c := newCluster(t, 1, true)
	old := c.leaderOf(t, "shard-1")
	old.stop()
	c.later(20 * time.Second)
	followers := c.others(old.id)
	c.beat(t, followers...)
	c.members.CheckLeaders(context.Background())
	promoted := c.members.Status()[0]

	c.startCoordinator(t) // a new coordinator over the same leader store
	c.beat(t, followers...)
	if s := c.members.Status()[0]; s.Leader != promoted.Leader || s.Epoch != promoted.Epoch {
		t.Fatalf("after restart: %s/%d, want %s/%d", s.Leader, s.Epoch, promoted.Leader, promoted.Epoch)
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
