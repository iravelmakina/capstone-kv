package shard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/store"
)

// Short timings so the failure scenarios run in well under a second each.
var testCfg = Config{Shard: "shard-1", W: 2, CommitTimeout: 300 * time.Millisecond,
	LongPoll: 200 * time.Millisecond, PullDeadline: time.Second, PullMax: 100}

// node is one replica with its store, HTTP server and pull loop; stop and start it like
// a process.
type node struct {
	id, dir string
	r       *Replica
	st      *store.Store
	srv     *httptest.Server
	cancel  context.CancelFunc
}

func startNode(t *testing.T, id, dir string) *node {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testCfg
	cfg.Replica = id
	n := &node{id: id, dir: dir, st: st, r: NewReplica(st, cfg)}
	n.srv = httptest.NewServer(n.r.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.r.Run(ctx)
	t.Cleanup(n.stop)
	return n
}

func (n *node) stop() {
	if n.srv == nil {
		return
	}
	n.cancel()
	n.srv.CloseClientConnections()
	n.srv.Close()
	n.st.Close()
	n.srv = nil
}

// restart starts a stopped node again on the same data directory.
func (n *node) restart(t *testing.T) *node {
	return startNode(t, n.id, n.dir)
}

// lead makes leader the leader of epoch for every node given.
func lead(epoch uint64, leader *node, all ...*node) {
	for _, n := range all {
		n.r.SetRole(epoch, leader.id, leader.srv.URL)
	}
}

func put(t *testing.T, n *node, key, value string) int {
	t.Helper()
	code, _ := do(t, "PUT", n.srv.URL+KeyPath(key), value)
	return code
}

func lastLSN(n *node) uint64 {
	v, _ := n.st.Last()
	return v.LSN
}

// eventually retries cond for up to 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func threeNodes(t *testing.T) (a, b, c *node) {
	a = startNode(t, "1a", t.TempDir())
	b = startNode(t, "1b", t.TempDir())
	c = startNode(t, "1c", t.TempDir())
	lead(1, a, a, b, c)
	return
}

// Spec "Follower stays current": 1,000 committed puts reach both followers within 5 s.
func TestFollowersStayCurrent(t *testing.T) {
	a, b, c := threeNodes(t)
	for i := 0; i < 1000; i++ {
		if code := put(t, a, fmt.Sprintf("k%d", i), `{"i":1}`); code != 200 {
			t.Fatalf("put %d = %d", i, code)
		}
	}
	eventually(t, "followers to reach the leader", func() bool {
		return lastLSN(b) == lastLSN(a) && lastLSN(c) == lastLSN(a)
	})
	if v, ok := c.st.Get("k999"); !ok || string(v) != `{"i":1}` {
		t.Fatal("follower missing the last put")
	}
}

// Spec "Write sent to a follower": 409, data unchanged.
func TestWriteToFollowerIsRefused(t *testing.T) {
	_, b, _ := threeNodes(t)
	if code := put(t, b, "k", `{}`); code != http.StatusConflict {
		t.Fatalf("put on a follower = %d, want 409", code)
	}
	if lastLSN(b) != 0 {
		t.Fatal("follower wrote to its log")
	}
}

// Spec "One follower down": a put still commits.
func TestOneFollowerDown(t *testing.T) {
	a, b, _ := threeNodes(t)
	b.stop()
	if code := put(t, a, "k", `{"v":1}`); code != 200 {
		t.Fatalf("put with one follower down = %d, want 200", code)
	}
}

// Spec "Both followers down": 504 within about the commit timeout, and the write isn't
// visible on the leader.
func TestBothFollowersDown(t *testing.T) {
	a, b, c := threeNodes(t)
	b.stop()
	c.stop()
	start := time.Now()
	code := put(t, a, "k", `{"v":1}`)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("put with both followers down = %d, want 504", code)
	}
	if took := time.Since(start); took > 3*testCfg.CommitTimeout {
		t.Fatalf("504 took %s", took)
	}
	if code, _ := do(t, "GET", a.srv.URL+KeyPath("k"), ""); code != http.StatusNotFound {
		t.Fatalf("uncommitted write visible on the leader (GET = %d)", code)
	}
}

// A write that timed out still commits once a follower catches up, and the leader then
// shows it (ADR-0023: "outcome unknown", not "lost").
func TestTimedOutWriteCommitsLater(t *testing.T) {
	a, b, c := threeNodes(t)
	c.stop()
	b.stop()
	put(t, a, "late", `{"v":1}`)
	b = b.restart(t)
	lead(1, a, b)
	eventually(t, "the late write to become visible on the leader", func() bool {
		_, ok := a.st.Get("late")
		return ok
	})
}

// Spec "Follower restarts behind": after a restart the first pull starts right after its
// last record, not at LSN 1.
func TestFollowerRestartResyncsOnlyTheGap(t *testing.T) {
	a, b, _ := threeNodes(t)
	for i := 0; i < 20; i++ {
		put(t, a, fmt.Sprintf("before%d", i), `{}`)
	}
	eventually(t, "b to catch up", func() bool { return lastLSN(b) == lastLSN(a) })
	b.stop()
	stoppedAt := lastLSN(b)
	for i := 0; i < 500; i++ {
		put(t, a, fmt.Sprintf("after%d", i), `{}`)
	}

	var mu sync.Mutex
	var first *uint64
	a.r.mu.Lock()
	a.r.onPull = func(replica string, lsn uint64) {
		mu.Lock()
		defer mu.Unlock()
		if replica == "1b" && first == nil {
			first = &lsn
		}
	}
	a.r.mu.Unlock()
	b = b.restart(t)
	lead(1, a, b)
	eventually(t, "b to catch up after its restart", func() bool { return lastLSN(b) == lastLSN(a) })
	mu.Lock()
	defer mu.Unlock()
	if first == nil || *first != stoppedAt {
		t.Fatalf("first pull after restart started at %v, want %d", first, stoppedAt)
	}
}

// Spec "Former leader returns with an uncommitted tail": A accepted a write no follower
// got, B was promoted, A comes back as a follower, drops that write and matches B.
func TestFormerLeaderDropsItsUncommittedTail(t *testing.T) {
	a, b, c := threeNodes(t)
	put(t, a, "x", `{"committed":true}`)
	eventually(t, "followers to have x", func() bool { return lastLSN(b) == 1 && lastLSN(c) == 1 })
	b.stop()
	c.stop()
	if code := put(t, a, "y", `{"committed":false}`); code != http.StatusGatewayTimeout {
		t.Fatalf("put without followers = %d, want 504", code)
	}
	a.stop()

	b, c = b.restart(t), c.restart(t)
	lead(2, b, b, c) // what the coordinator's promotion does (group 4)
	if code := put(t, b, "z", `{"newLeader":true}`); code != 200 {
		t.Fatalf("put on the new leader = %d", code)
	}

	a = a.restart(t)
	lead(2, b, a)
	eventually(t, "a to match the new leader", func() bool {
		av, _ := a.st.Last()
		bv, _ := b.st.Last()
		return av == bv
	})
	if _, ok := a.st.Get("y"); ok {
		t.Fatal("the uncommitted write survived on the former leader")
	}
	if _, ok := a.st.Get("z"); !ok {
		t.Fatal("former leader missed the new leader's write")
	}
	if code := put(t, a, "w", `{}`); code != http.StatusConflict {
		t.Fatalf("former leader accepted a write: %d", code)
	}
}

// Reads carry the version; a tombstone is 404 with its version; a missing key has none.
func TestReadsCarryVersions(t *testing.T) {
	a, _, _ := threeNodes(t)
	put(t, a, "k", `{"v":1}`)
	req, _ := http.NewRequest("GET", a.srv.URL+KeyPath("k"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get(HeaderEpoch) != "1" || resp.Header.Get(HeaderLSN) != "1" {
		t.Fatalf("version headers %q/%q, want 1/1", resp.Header.Get(HeaderEpoch), resp.Header.Get(HeaderLSN))
	}
	do(t, "DELETE", a.srv.URL+KeyPath("k"), "")
	resp, _ = http.Get(a.srv.URL + KeyPath("k"))
	resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get(HeaderLSN) != "2" {
		t.Fatalf("tombstone: status %d, LSN %q; want 404 at LSN 2", resp.StatusCode, resp.Header.Get(HeaderLSN))
	}
	resp, _ = http.Get(a.srv.URL + KeyPath("never"))
	resp.Body.Close()
	if resp.StatusCode != 404 || resp.Header.Get(HeaderLSN) != "" {
		t.Fatal("a key never written must have no version")
	}
}

// Spec "Lag visible": a follower that was down shows a lag that falls back to 0; the
// leader reports its commit waits.
func TestHealthShowsRoleLagAndCommitWait(t *testing.T) {
	a, b, _ := threeNodes(t)
	put(t, a, "k", `{}`)
	b.stop()
	for i := 0; i < 50; i++ {
		put(t, a, fmt.Sprintf("k%d", i), `{}`)
	}
	b = b.restart(t)
	lead(1, a, b)
	eventually(t, "b to catch up", func() bool { return lastLSN(b) == lastLSN(a) })

	var hs Status
	_, body := do(t, "GET", b.srv.URL+"/health", "")
	json.Unmarshal([]byte(body), &hs)
	if hs.Role != "follower" || hs.Leader != "1a" || hs.Lag != 0 || hs.Last.LSN != lastLSN(a) {
		t.Fatalf("follower health %+v", hs)
	}
	_, body = do(t, "GET", a.srv.URL+"/health", "")
	json.Unmarshal([]byte(body), &hs)
	if hs.Role != "leader" || hs.Epoch != 1 || hs.CommitMs.P99 <= 0 {
		t.Fatalf("leader health %+v", hs)
	}
}

// An answer about an older epoch never undoes a newer one.
func TestOlderEpochIgnored(t *testing.T) {
	a, b, _ := threeNodes(t)
	lead(2, b, a, b)
	lead(1, a, a, b)
	if s := a.r.Status(); s.Epoch != 2 || s.Leader != "1b" {
		t.Fatalf("after a stale answer: %+v", s)
	}
}

// The log endpoint: only the current leader serves it, and it checks its parameters.
func TestLogEndpoint(t *testing.T) {
	a, b, _ := threeNodes(t)
	if code, _ := do(t, "GET", b.srv.URL+"/internal/v1/log?replica=1c&epoch=0&lsn=0&hash=0&max=10", ""); code != http.StatusConflict {
		t.Fatalf("pull from a follower = %d, want 409", code)
	}
	if code, _ := do(t, "GET", a.srv.URL+"/internal/v1/log?replica=1c&lsn=x", ""); code != http.StatusBadRequest {
		t.Fatalf("bad pull = %d, want 400", code)
	}
}

// HeartbeatLoop takes the replica's role from the coordinator's answer.
func TestHeartbeatTakesRoleFromTheAnswer(t *testing.T) {
	n := startNode(t, "1a", t.TempDir())
	var got Heartbeat
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(HeartbeatAnswer{Epoch: 3, Leader: "1a", LeaderAddress: n.srv.URL})
	}))
	defer coord.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.r.HeartbeatLoop(ctx, coord.URL, n.srv.URL, time.Hour)
	eventually(t, "the role from the heartbeat answer", func() bool { return n.r.Status().Role == "leader" })
	if got.Shard != "shard-1" || got.Replica != "1a" || got.Address != n.srv.URL {
		t.Fatalf("heartbeat sent %+v", got)
	}
	if code := put(t, n, "k", `{}`); code != http.StatusGatewayTimeout {
		t.Fatalf("leader without followers, W=2: put = %d, want 504", code)
	}
}

// metricValue returns the value of the first series line starting with prefix.
func metricValue(t *testing.T, n *node, prefix string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	n.r.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatalf("bad line %q", line)
			}
			return v
		}
	}
	return -1
}

// Spec "Lag visible on the leader": a stopped follower's lag grows on the leader, the
// live one stays at 0; the leader also times its writes and commit waits.
func TestFollowerLagOnLeader(t *testing.T) {
	a, b, c := threeNodes(t)
	if code := put(t, a, "k0", `{"v":0}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	eventually(t, "followers caught up", func() bool { return lastLSN(b) == 1 && lastLSN(c) == 1 })
	c.stop()
	for i := 1; i <= 3; i++ {
		put(t, a, fmt.Sprintf("k%d", i), `{"v":1}`)
	}
	time.Sleep(300 * time.Millisecond)
	lagC := metricValue(t, a, `kv_follower_lag_seconds{shard="shard-1",replica="1a",follower="1c"}`)
	if lagC < 0.2 {
		t.Errorf("stopped follower's lag = %v s, want ≥ 0.2", lagC)
	}
	eventually(t, "live follower at 0 lag", func() bool {
		return metricValue(t, a, `kv_follower_lag_seconds{shard="shard-1",replica="1a",follower="1b"}`) == 0
	})
	if n := metricValue(t, a, `kv_commit_wait_seconds_count{shard="shard-1",replica="1a"}`); n != 4 {
		t.Errorf("commit waits counted %v, want 4", n)
	}
	if n := metricValue(t, a, `kv_replica_request_duration_seconds_count{shard="shard-1",replica="1a",op="write"}`); n != 4 {
		t.Errorf("writes timed %v, want 4", n)
	}
	if v := metricValue(t, a, `kv_replica_role{shard="shard-1",replica="1a",role="leader"}`); v != 1 {
		t.Errorf("role metric = %v", v)
	}
}

// A coordinator that has just restarted may answer a heartbeat with the leader's name but
// no address yet (the leader hasn't reported in). The follower must keep pulling from the
// address it knows, and a changed address must wake its pull loop (found in the M4 drill:
// after a coordinator restart every write got 504).
func TestFollowerKeepsPullingAfterEmptyLeaderAddress(t *testing.T) {
	a, b, c := threeNodes(t)
	if code := put(t, a, "k0", `{"v":0}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	b.r.SetRole(1, a.id, "") // the restarted coordinator's first answer
	c.r.SetRole(1, a.id, "")
	if code := put(t, a, "k1", `{"v":1}`); code != 200 {
		t.Fatalf("put after an answer without the leader's address = %d, want 200", code)
	}
	eventually(t, "followers at the leader's LSN", func() bool { return lastLSN(b) == 2 && lastLSN(c) == 2 })
}
