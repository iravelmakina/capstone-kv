package coordinator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// Replica-set states (ADR-0033, spec shard-rebalancing "Pending shards").

func stateOf(m *Membership, id string) (string, bool) {
	for _, s := range m.Status() {
		if s.ID == id {
			return s.State, s.Base
		}
	}
	return "", false
}

// Spec "Registration doesn't remap keys": shard-3 registers while shards 1 and 2 hold
// data; it is pending, no key routes to it, every key still reads back, and the store
// stays ready although shard-3 is still choosing its leader.
func TestLateSetIsPendingAndRemapsNoKey(t *testing.T) {
	c := newCluster(t, 2, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	for i := 0; i < 200; i++ {
		code, body := call(t, "PUT", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), fmt.Sprintf(`{"n":%d}`, i))
		expect(t, code, body, 200)
	}

	for _, id := range []string{"3a", "3b"} { // two of three: no leader yet (FirstLeaderWait)
		c.startNode(t, "shard-3", id)
	}
	c.beat(t, "3a", "3b")
	if st, base := stateOf(c.members, "shard-3"); st != StatePending || base {
		t.Fatalf("shard-3 state %q base %v, want pending, not base", st, base)
	}
	if ready, n := c.members.Ready(); !ready || n != 2 {
		t.Fatalf("Ready = %v with %d sets on the ring; a pending set must not count", ready, n)
	}

	for i := 0; i < 200; i++ {
		key := ring.RoutingKey("users", fmt.Sprintf("u%d", i))
		if id, _, _ := c.members.WriteTarget(key); id == "shard-3" {
			t.Fatalf("u%d routes to the pending shard-3", i)
		}
		code, body := call(t, "GET", fmt.Sprintf("%s/v1/tables/users/items/u%d", c.coord.URL, i), "")
		if code != 200 || body != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("u%d after shard-3 registered: %d %q", i, code, body)
		}
	}
	metrics := scrapeMetrics(t, c.obj.metrics)
	for _, want := range []string{"kv_ring_shards 2", `kv_shard_state{shard="shard-3",state="pending"} 1`, `kv_shard_state{shard="shard-1",state="active"} 1`} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics lack %q", want)
		}
	}
}

// States survive a coordinator restart: a pending set stays pending (and off the ring)
// even though, after a restart, it may report before the base sets do; an M4 record with
// no state loads as an active base set.
func TestSetStatesSurviveRestart(t *testing.T) {
	leaders := openStore(t)
	m4, _ := json.Marshal(map[string]any{"leader": "1a", "epoch": 3}) // as M4 wrote it
	leaders.Put("shard-1", m4)
	newM := func() *Membership {
		m, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
			Timeout: 15 * time.Second, ExpectedShards: 2, Replicas: 3, Leaders: leaders,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	hb := func(m *Membership, s, r string) {
		if _, err := m.Heartbeat(shard.Heartbeat{Shard: s, Replica: r, Address: "http://" + r + ":8081",
			Last: store.Version{Epoch: 1, LSN: 1}}); err != nil {
			t.Fatal(err)
		}
	}

	m := newM()
	if st, base := stateOf(m, "shard-1"); st != StateActive || !base {
		t.Fatalf("M4 record loads as %q base %v, want active base", st, base)
	}
	hb(m, "shard-2", "2a") // the second base set
	hb(m, "shard-3", "3a") // beyond ExpectedShards: pending

	m = newM() // restart; shard-3 reports first this time
	hb(m, "shard-3", "3a")
	if st, base := stateOf(m, "shard-3"); st != StatePending || base {
		t.Fatalf("after restart shard-3 is %q base %v, want pending", st, base)
	}
	if st, _ := stateOf(m, "shard-2"); st != StateActive {
		t.Fatalf("after restart shard-2 is %q, want active", st)
	}
	for i := 0; i < 100; i++ {
		if id, found := m.ring.Lookup([]byte(fmt.Sprintf("k%d", i))); found && id == "shard-3" {
			t.Fatal("a pending set is on the ring after a restart")
		}
	}
}
