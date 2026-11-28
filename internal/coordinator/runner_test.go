package coordinator

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
)

// The migration runner end to end (ADR-0033, spec shard-rebalancing, task 5.4), on an
// in-process cluster: shard-1 and shard-2 with data, shard-3 joining and leaving.

var fastRunner = RunnerConfig{Rate: 1_000_000, Page: 500, Tick: 20 * time.Millisecond, Backoff: 100 * time.Millisecond}

// keepAlive sends heartbeats for every replica every second, so long tests don't see them
// time out (the harness's beat() is manual).
func (c *cluster) keepAlive(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			for id, n := range c.nodes {
				s := n.r.Status()
				if a, err := c.members.Heartbeat(shard.Heartbeat{Shard: n.shard, Replica: id, Address: n.srv.URL, Epoch: s.Epoch, Last: s.Last}); err == nil {
					n.r.SetRole(a.Epoch, a.Leader, a.LeaderAddress)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
}

// withShard3 starts a cluster of shard-1 and shard-2 with a table and n items, and a
// pending shard-3 with its leader chosen, all kept alive by heartbeats.
func withShard3(t *testing.T, n int) *cluster {
	t.Helper()
	c := newCluster(t, 2, true)
	for _, id := range []string{"3a", "3b", "3c"} { // pending: no key goes to it yet
		c.startNode(t, "shard-3", id)
	}
	c.beat(t, "3a", "3b", "3c")
	alive, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	c.keepAlive(alive) // after every node exists (it reads c.nodes); seeding can outlast the heartbeat timeout
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += 8 {
				code, body := call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), fmt.Sprintf(`{"n":%d}`, i))
				if code != 200 {
					errs <- fmt.Sprintf("seed u%d: %d %s", i, code, body)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	return c
}

func (c *cluster) leaderKeys(t *testing.T, shardID string) int {
	t.Helper()
	return c.leaderOf(t, shardID).r.Status().Keys
}

func waitPhase(t *testing.T, m *Membership, phase string, within time.Duration) *Migration {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		mg := m.CurrentMigration()
		if mg != nil && mg.Phase == phase {
			return mg
		}
		if time.Now().After(deadline) {
			t.Fatalf("migration not %s within %s: %+v", phase, within, mg)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writers put keys through the coordinator until stopped, each writer its own keys
// (so "the last value acknowledged" is well defined), and record what was acknowledged
// and every 5xx.
type writers struct {
	mu     sync.Mutex
	acked  map[string]string
	fivexx []string
	slow   time.Duration // the longest write
	stop   chan struct{}
	wg     sync.WaitGroup
}

func startWriters(t *testing.T, c *cluster, n int) *writers {
	w := &writers{acked: map[string]string{}, stop: make(chan struct{})}
	for g := 0; g < n; g++ {
		w.wg.Add(1)
		go func(g int) {
			defer w.wg.Done()
			for i := 0; ; i++ {
				select {
				case <-w.stop:
					return
				default:
				}
				key := fmt.Sprintf("w%d-%d", g, i%300) // keys are rewritten: updates as well as inserts
				val := fmt.Sprintf(`{"g":%d,"i":%d}`, g, i)
				start := time.Now()
				code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/"+key, val)
				took := time.Since(start)
				w.mu.Lock()
				w.slow = max(w.slow, took)
				switch {
				case code == 200:
					w.acked[key] = val
				case code >= 500:
					w.fivexx = append(w.fivexx, fmt.Sprintf("%s: %d %s", key, code, body))
				}
				w.mu.Unlock()
			}
		}(g)
	}
	return w
}

func (w *writers) halt() { close(w.stop); w.wg.Wait() }

func migrationKeys() int {
	if n, err := strconv.Atoi(os.Getenv("KV_MIGRATION_KEYS")); err == nil && n > 0 {
		return n // e.g. KV_MIGRATION_KEYS=100000 for the full-size run of task 5.4
	}
	return 3000
}

// Task 5.4's main test: shard-3 joins and then leaves while writers run. Only the moving
// keys move, every acknowledged write reads back with its value, no write gets a 5xx or
// waits more than the cutover's budget, the key counts add up, and the old shards shrink.
func TestMigrationAddThenRemoveUnderLoad(t *testing.T) {
	n := migrationKeys()
	c := withShard3(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.members.RunMigrations(ctx, fastRunner)
	before1, before2 := c.leaderKeys(t, "shard-1"), c.leaderKeys(t, "shard-2")

	w := startWriters(t, c, 4)
	time.Sleep(100 * time.Millisecond)
	if _, err := c.members.StartMigration(MigrationRequest{Add: "shard-3"}, "test"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, c.members, PhaseDone, 2*time.Minute)
	time.Sleep(100 * time.Millisecond)
	w.halt()

	if st, _ := stateOf(c.members, "shard-3"); st != StateActive || fmt.Sprint(c.members.RingShards()) != "[shard-1 shard-2 shard-3]" {
		t.Fatalf("after the add: shard-3 %s, ring %v", st, c.members.RingShards())
	}
	k1, k2, k3 := c.leaderKeys(t, "shard-1"), c.leaderKeys(t, "shard-2"), c.leaderKeys(t, "shard-3")
	total := n + len(w.acked)
	if k1+k2+k3 != total {
		t.Fatalf("keys per shard %d+%d+%d = %d, want %d (no key lost or doubled)", k1, k2, k3, k1+k2+k3, total)
	}
	if k1 >= before1 || k2 >= before2 || k3 < total/5 {
		t.Fatalf("distribution after the add: %d %d %d (before %d %d)", k1, k2, k3, before1, before2)
	}
	t.Logf("add: keys per shard %d / %d / %d (before %d / %d); longest write %s", k1, k2, k3, before1, before2, w.slow.Round(time.Millisecond))
	checkAll(t, c, n, w)

	// And back: shard-3 leaves.
	w2 := startWriters(t, c, 4)
	time.Sleep(100 * time.Millisecond)
	if _, err := c.members.StartMigration(MigrationRequest{Remove: "shard-3"}, "test"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, c.members, PhaseDone, 2*time.Minute)
	time.Sleep(100 * time.Millisecond)
	w2.halt()
	if st, _ := stateOf(c.members, "shard-3"); st != StateRetired || fmt.Sprint(c.members.RingShards()) != "[shard-1 shard-2]" {
		t.Fatalf("after the removal: shard-3 %s, ring %v", st, c.members.RingShards())
	}
	if k3 := c.leaderKeys(t, "shard-3"); k3 != 0 {
		t.Fatalf("retired shard-3 still holds %d keys", k3)
	}
	for k, v := range w2.acked {
		w.acked[k] = v
	}
	w.fivexx, w.slow = append(w.fivexx, w2.fivexx...), max(w.slow, w2.slow)
	if k1, k2 := c.leaderKeys(t, "shard-1"), c.leaderKeys(t, "shard-2"); k1+k2 != n+len(w.acked) {
		t.Fatalf("after the removal: %d + %d keys, want %d", k1, k2, n+len(w.acked))
	}
	checkAll(t, c, n, w)
}

// checkAll: no 5xx, no write over the budget, every seeded and acknowledged key reads back.
func checkAll(t *testing.T, c *cluster, n int, w *writers) {
	t.Helper()
	if len(w.fivexx) > 0 {
		t.Fatalf("%d writes got a 5xx, first: %s", len(w.fivexx), w.fivexx[0])
	}
	if w.slow > 2500*time.Millisecond {
		t.Fatalf("a write took %s, more than the 2 s cutover budget", w.slow)
	}
	for i := 0; i < n; i++ {
		code, body := call(t, "GET", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), "")
		if code != 200 || body != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("seeded u%d reads back %d %q", i, code, body)
		}
	}
	for k, v := range w.acked {
		if code, body := call(t, "GET", c.coord.URL+"/v1/tables/users/items/"+k, ""); code != 200 || body != v {
			t.Fatalf("acknowledged %s=%s reads back %d %q", k, v, code, body)
		}
	}
}

// Spec "In-flight write at cutover" (AI review W1): writes injected into the windows of a
// cutover. Through the coordinator while held: they wait and land on the new owner.
// Straight to the source before its freeze: accepted, and they reach the new owner through
// the tail. Straight to the source after its freeze: refused with 409 "moved", never
// applied anywhere.
func TestCutoverWithInjectedWrites(t *testing.T) {
	c := withShard3(t, 300)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	after := ring.Of(ring.DefaultVirtualNodes, []string{"shard-1", "shard-2", "shard-3"})
	before := ring.Of(ring.DefaultVirtualNodes, []string{"shard-1", "shard-2"})
	movingFrom := func(src string, skip int) string { // the skip-th key moving src -> shard-3
		for i := 0; ; i++ {
			pk := fmt.Sprintf("x%d", i)
			rk := ring.RoutingKey("users", pk)
			if b, _ := before.Lookup(rk); b == src {
				if a, _ := after.Lookup(rk); a == "shard-3" {
					if skip == 0 {
						return pk
					}
					skip--
				}
			}
		}
	}
	direct := func(src, pk, value string) int {
		addr, _ := c.members.leaderAddr(src)
		code, _ := call(t, "PUT", addr+shard.KeyPath(shard.StorageKey("users", pk, "")), value)
		return code
	}
	type result struct {
		pk, value string
		code      int
	}
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	viaCoord := func(pk, value string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/"+pk, value)
			mu.Lock()
			results = append(results, result{pk, value, code})
			mu.Unlock()
		}()
	}
	frozenPK := map[string]string{}
	for _, src := range []string{"shard-1", "shard-2"} {
		pk := movingFrom(src, 2)
		frozenPK[src] = pk
		call(t, "PUT", c.coord.URL+"/v1/tables/users/items/"+pk, `{"v":"old"}`)
	}

	cfg := fastRunner
	cfg.hooks = &runnerHooks{
		afterHold: func(src string) {
			viaCoord(movingFrom(src, 0), `{"v":"held"}`)
			if code := direct(src, movingFrom(src, 1), `{"v":"before-freeze"}`); code != 200 {
				t.Errorf("direct write before the freeze on %s: %d, want 200", src, code)
			}
			mu.Lock()
			results = append(results, result{movingFrom(src, 1), `{"v":"before-freeze"}`, 200})
			mu.Unlock()
		},
		afterFreeze: func(src string) {
			if code := direct(src, frozenPK[src], `{"v":"after-freeze"}`); code != 409 {
				t.Errorf("direct write after the freeze on %s: %d, want 409 moved", src, code)
			}
			viaCoord(movingFrom(src, 3), `{"v":"held-after-freeze"}`)
		},
	}
	go c.members.RunMigrations(ctx, cfg)
	if _, err := c.members.StartMigration(MigrationRequest{Add: "shard-3"}, "test"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, c.members, PhaseDone, time.Minute)
	wg.Wait()

	for _, r := range results {
		if r.code != 200 {
			t.Fatalf("%s: write answered %d, want 200", r.pk, r.code)
		}
		if code, body := call(t, "GET", c.coord.URL+"/v1/tables/users/items/"+r.pk, ""); code != 200 || body != r.value {
			t.Fatalf("%s reads %d %q, want %q", r.pk, code, body, r.value)
		}
		if id, _, _ := c.members.WriteTarget(ring.RoutingKey("users", r.pk)); id != "shard-3" {
			t.Fatalf("%s is served by %s, want shard-3", r.pk, id)
		}
	}
	for src, pk := range frozenPK {
		if _, body := call(t, "GET", c.coord.URL+"/v1/tables/users/items/"+pk, ""); body != `{"v":"old"}` {
			t.Fatalf("%s (refused on %s after its freeze) reads %q, want the old value", pk, src, body)
		}
	}
	if len(results) != 6 {
		t.Fatalf("%d injected writes recorded, want 6 (both sources cut over)", len(results))
	}
}

// Abort before any cutover: the target drops what it got, the sources aren't frozen, and
// the old ring serves every key.
func TestAbortCleansUp(t *testing.T) {
	c := withShard3(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := fastRunner
	cfg.Rate = 2000 // slow enough to abort in the middle
	go c.members.RunMigrations(ctx, cfg)
	if _, err := c.members.StartMigration(MigrationRequest{Add: "shard-3"}, "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.leaderKeys(t, "shard-3") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing copied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.members.AbortMigration("test"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for !c.members.CurrentMigration().CleanedUp {
		if time.Now().After(deadline) {
			t.Fatal("not cleaned up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if k := c.leaderKeys(t, "shard-3"); k != 0 {
		t.Fatalf("aborted target still holds %d keys", k)
	}
	if st, _ := stateOf(c.members, "shard-3"); st != StatePending {
		t.Fatalf("shard-3 is %s after the abort, want pending", st)
	}
	for i := 0; i < 1000; i += 7 {
		code, body := call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), `{"after":"abort"}`)
		if code != 200 {
			t.Fatalf("write after the abort: %d %s (a source left frozen?)", code, body)
		}
	}
}

// The runner stops in the middle and a new one finishes the migration from the persisted
// progress (what a coordinator restart does; the record's reload is TestMigrationSurvivesRestart).
func TestRunnerResumes(t *testing.T) {
	c := withShard3(t, 2000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, stopFirst := context.WithCancel(ctx)
	cfg := fastRunner
	cfg.Rate, cfg.Page = 3000, 100
	go c.members.RunMigrations(first, cfg)
	if _, err := c.members.StartMigration(MigrationRequest{Add: "shard-3"}, "test"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		mg := c.members.CurrentMigration()
		if mg.Sources["shard-1"].SnapshotDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first source's snapshot not done")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopFirst()
	time.Sleep(200 * time.Millisecond)
	if mg := c.members.CurrentMigration(); mg.Phase == PhaseDone {
		t.Skip("finished before it could be stopped")
	}
	go c.members.RunMigrations(ctx, fastRunner)
	waitPhase(t, c.members, PhaseDone, time.Minute)
	for i := 0; i < 2000; i++ {
		code, body := call(t, "GET", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), "")
		if code != 200 || !strings.Contains(body, fmt.Sprintf(`"n":%d`, i)) {
			t.Fatalf("u%d after a resumed migration: %d %q", i, code, body)
		}
	}
}
