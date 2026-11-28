package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// Migration records and routing (ADR-0033, spec shard-rebalancing); the key movement
// itself is tested with the runner.

// migCluster is a Membership with base sets shard-1 and shard-2 and a pending shard-3, each
// with three live replicas and a confirmed leader; leaders persist in leaders.
func migCluster(t *testing.T, leaders *store.Store) *Membership {
	t.Helper()
	m, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: 2, Replicas: 3, Leaders: leaders,
	})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ { // round 1: leaders chosen; round 2: confirmed
		for _, s := range []string{"1", "2", "3"} {
			for _, l := range []string{"a", "b", "c"} {
				st := m.shards["shard-"+s]
				var epoch uint64
				if st != nil {
					epoch = st.epoch
				}
				if _, err := m.Heartbeat(shard.Heartbeat{Shard: "shard-" + s, Replica: s + l,
					Address: "http://" + s + l + ":8081", Epoch: epoch}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return m
}

func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

func keyN(i int) []byte { return ring.RoutingKey("users", fmt.Sprintf("u%d", i)) }

// Spec "Old owner serves until its cutover": while copying, every key routes as before;
// after one source cuts over, only that source's moving keys go to the new set.
func TestMigrationRouting(t *testing.T) {
	m := migCluster(t, openStore(t))
	before := map[int]string{}
	for i := 0; i < 2000; i++ {
		before[i], _, _ = m.WriteTarget(keyN(i))
	}
	mg, err := m.StartMigration(MigrationRequest{Add: "shard-3"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if mg.Phase != PhaseCopying || len(mg.Sources) != 2 || fmt.Sprint(mg.After) != "[shard-1 shard-2 shard-3]" {
		t.Fatalf("migration %+v", mg)
	}
	after := ring.Of(ring.DefaultVirtualNodes, mg.After)
	for i := 0; i < 2000; i++ {
		if id, _, _ := m.WriteTarget(keyN(i)); id != before[i] {
			t.Fatalf("u%d routes to %s during the copy, want its old owner %s", i, id, before[i])
		}
	}

	m.mu.Lock()
	m.mig.Sources["shard-1"].CutOver = true
	m.mu.Unlock()
	moved := 0
	for i := 0; i < 2000; i++ {
		id, _, _ := m.WriteTarget(keyN(i))
		rid, _ := m.ReadTargets(keyN(i))
		newOwner, _ := after.Lookup(keyN(i))
		want := before[i]
		if before[i] == "shard-1" && newOwner == "shard-3" {
			want = "shard-3"
			moved++
		}
		if id != want || rid != want {
			t.Fatalf("u%d: writes to %s, reads from %s, want %s", i, id, rid, want)
		}
	}
	if moved < 200 || moved > 500 { // about a sixth of 2,000 (a third of shard-1's half)
		t.Fatalf("%d keys moved from shard-1, want about 333", moved)
	}
}

// A running migration survives a coordinator restart, routing included.
func TestMigrationSurvivesRestart(t *testing.T) {
	leaders := openStore(t)
	m := migCluster(t, leaders)
	if _, err := m.StartMigration(MigrationRequest{Add: "shard-3", TriggerRate: 812.5}, "kvscale"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.mig.Sources["shard-2"].CutOver = true
	m.saveMigrationLocked(m.mig)
	m.mu.Unlock()
	want := map[int]string{}
	for i := 0; i < 500; i++ {
		want[i], _, _ = m.WriteTarget(keyN(i))
	}

	m2, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: 2, Replicas: 3, Leaders: leaders,
	})
	if err != nil {
		t.Fatal(err)
	}
	mg := m2.CurrentMigration()
	if !mg.Running() || mg.TriggerRate != 812.5 || mg.RequestedBy != "kvscale" || !mg.Sources["shard-2"].CutOver {
		t.Fatalf("after restart: %+v", mg)
	}
	m2.now = func() time.Time { return time.Now() }
	for _, s := range []string{"1", "2", "3"} { // replicas report again
		for _, l := range []string{"a", "b", "c"} {
			m2.Heartbeat(shard.Heartbeat{Shard: "shard-" + s, Replica: s + l, Address: "http://" + s + l + ":8081", Epoch: 1})
		}
	}
	for i := 0; i < 500; i++ {
		if id, _, _ := m2.WriteTarget(keyN(i)); id != want[i] {
			t.Fatalf("u%d routes to %s after restart, %s before", i, id, want[i])
		}
	}
}

// Spec "Second migration refused", "Base set can't leave" and the other refusals.
func TestMigrationRefusals(t *testing.T) {
	m := migCluster(t, openStore(t))
	for _, tc := range []struct {
		req  MigrationRequest
		want int
	}{
		{MigrationRequest{}, 400},
		{MigrationRequest{Add: "shard-3", Remove: "shard-3"}, 400},
		{MigrationRequest{Add: "shard-9"}, 404},
		{MigrationRequest{Add: "shard-1"}, 409},    // active, not pending
		{MigrationRequest{Remove: "shard-1"}, 409}, // a base set
		{MigrationRequest{Remove: "shard-3"}, 409}, // pending, not active
	} {
		if _, err := m.StartMigration(tc.req, "t"); statusOf(err) != tc.want {
			t.Errorf("%+v: %v, want %d", tc.req, err, tc.want)
		}
	}
	if _, err := m.StartMigration(MigrationRequest{Add: "shard-3"}, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartMigration(MigrationRequest{Add: "shard-3"}, "t"); statusOf(err) != 409 {
		t.Fatalf("second migration: %v, want 409", err)
	}
	if err := m.Remove("shard-3"); statusOf(err) != 409 {
		t.Fatalf("forgetting a set in migration: %v, want 409", err)
	}
}

// A pending set without a leader can't join yet.
func TestAddNeedsALeader(t *testing.T) {
	m := migCluster(t, openStore(t))
	m.Heartbeat(shard.Heartbeat{Shard: "shard-4", Replica: "4a", Address: "http://4a:8081"})
	if _, err := m.StartMigration(MigrationRequest{Add: "shard-4"}, "t"); statusOf(err) != 409 {
		t.Fatalf("add without a leader: %v, want 409", err)
	}
}

// Abort: possible before any cutover (routing back to the old ring, a leaving set active
// again), refused after one, 404 with nothing running.
func TestAbortMigration(t *testing.T) {
	m := migCluster(t, openStore(t))
	if _, err := m.AbortMigration("x"); statusOf(err) != 404 {
		t.Fatalf("abort with nothing running: %v, want 404", err)
	}
	m.StartMigration(MigrationRequest{Add: "shard-3"}, "t")
	mg, err := m.AbortMigration("test")
	if err != nil || mg.Phase != PhaseAborted || mg.Finished.IsZero() {
		t.Fatalf("abort: %+v %v", mg, err)
	}
	for i := 0; i < 500; i++ {
		if id, _, _ := m.WriteTarget(keyN(i)); id == "shard-3" {
			t.Fatal("a key routes to shard-3 after the abort")
		}
	}

	m.StartMigration(MigrationRequest{Add: "shard-3"}, "t")
	m.mu.Lock()
	m.mig.Sources["shard-1"].CutOver = true
	m.mu.Unlock()
	if _, err := m.AbortMigration("late"); statusOf(err) != 409 {
		t.Fatalf("abort after a cutover: %v, want 409", err)
	}
}

// A remove marks the set leaving; aborting it makes it active again.
func TestRemoveMigrationStates(t *testing.T) {
	m := migCluster(t, openStore(t))
	m.mu.Lock() // as if shard-3 had joined earlier
	m.shards["shard-3"].state = StateActive
	m.ring.Add("shard-3")
	m.mu.Unlock()
	mg, err := m.StartMigration(MigrationRequest{Remove: "shard-3"}, "t")
	if err != nil || len(mg.Sources) != 1 || mg.Sources["shard-3"] == nil || fmt.Sprint(mg.After) != "[shard-1 shard-2]" {
		t.Fatalf("remove: %+v %v", mg, err)
	}
	if st, _ := stateOf(m, "shard-3"); st != StateLeaving {
		t.Fatalf("shard-3 is %q during its removal, want leaving", st)
	}
	m.AbortMigration("test")
	if st, _ := stateOf(m, "shard-3"); st != StateActive {
		t.Fatalf("shard-3 is %q after the aborted removal, want active", st)
	}
}

// The admin routes with keys: status may read the cluster but not change it or read
// items; admin may do both; no key is 401.
func TestAdminScopes(t *testing.T) {
	c := newCluster(t, 2, true)
	send := keyed(t, c)
	k := c.coord.URL
	send("POST", k+"/v1/tables", `{"name":"users","partitionKey":"userId"}`, "ops-key")
	for _, s := range []struct {
		method, path, body, key string
		want                    int
	}{
		{"GET", "/v1/admin/cluster", "", "status-key", 200},
		{"GET", "/v1/admin/cluster", "", "ops-key", 200}, // admin includes status
		{"GET", "/v1/admin/cluster", "", "", 401},
		{"GET", "/v1/admin/migrations/current", "", "status-key", 404}, // allowed; none yet
		{"POST", "/v1/admin/migrations", `{"add":"shard-9"}`, "status-key", 403},
		{"DELETE", "/v1/admin/migrations/current", "", "status-key", 403},
		{"POST", "/v1/admin/shards/shard-1/leader", `{"to":"1b"}`, "status-key", 403},
		{"GET", "/v1/tables/users/items/u1", "", "status-key", 403},
		{"POST", "/v1/admin/migrations", `{"add":"shard-9"}`, "ops-key", 404},
	} {
		if code, body := send(s.method, k+s.path, s.body, s.key); code != s.want {
			t.Errorf("%s %s with %q: %d %s, want %d", s.method, s.path, s.key, code, body, s.want)
		}
	}
	code, body := send("GET", k+"/v1/admin/cluster", "", "status-key")
	var cs ClusterState
	if json.Unmarshal([]byte(body), &cs); code != http.StatusOK || fmt.Sprint(cs.Ring) != "[shard-1 shard-2]" || len(cs.Sets) != 2 {
		t.Fatalf("cluster state: %d %s", code, body)
	}
}
