package ring

import (
	"fmt"
	"testing"
)

// testKeys returns n routing keys in table "usertable" with sequential partition keys
// "user0", "user1", ...: very similar strings, a harder case for the hash than the
// benchmark's own keys (YCSB with insertorder=hashed uses "user" + a hashed number).
func testKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = RoutingKey("usertable", fmt.Sprintf("user%d", i))
	}
	return keys
}

func ringOf(vnodes int, ids ...string) *Ring {
	r := New(vnodes)
	for _, id := range ids {
		r.Add(id)
	}
	return r
}

func TestEmptyRingHasNoOwner(t *testing.T) {
	if _, ok := New(DefaultVirtualNodes).Lookup([]byte("k")); ok {
		t.Fatal("empty ring returned an owner")
	}
}

func TestLookupIsStable(t *testing.T) {
	r := ringOf(DefaultVirtualNodes, "shard-1", "shard-2")
	for _, k := range testKeys(1000) {
		a, _ := r.Lookup(k)
		b, _ := r.Lookup(k)
		if a != b {
			t.Fatalf("key %q mapped to %s, then %s", k, a, b)
		}
	}
}

// Spec "Restart doesn't move keys": positions depend only on the shard ID.
func TestRestartWithSameIDMovesNothing(t *testing.T) {
	r := ringOf(DefaultVirtualNodes, "shard-1", "shard-2")
	keys := testKeys(10000)
	before := make([]string, len(keys))
	for i, k := range keys {
		before[i], _ = r.Lookup(k)
	}
	r.Remove("shard-2")
	r.Add("shard-2") // same ID, as after a restart at a new address
	for i, k := range keys {
		if after, _ := r.Lookup(k); after != before[i] {
			t.Fatalf("key %q moved from %s to %s", k, before[i], after)
		}
	}
}

// Spec "Few keys move when a shard joins": about 1/3 move, and only to the new shard.
func TestJoinMovesAboutOneThirdOnlyToNewShard(t *testing.T) {
	r := ringOf(DefaultVirtualNodes, "shard-1", "shard-2")
	keys := testKeys(100000)
	before := make([]string, len(keys))
	for i, k := range keys {
		before[i], _ = r.Lookup(k)
	}
	r.Add("shard-3")
	moved := 0
	for i, k := range keys {
		after, _ := r.Lookup(k)
		if after == before[i] {
			continue
		}
		if after != "shard-3" {
			t.Fatalf("key %q moved between old shards: %s -> %s", k, before[i], after)
		}
		moved++
	}
	frac := float64(moved) / float64(len(keys))
	if frac < 0.25 || frac > 0.42 {
		t.Fatalf("moved %.1f%% of keys, want 25-42%%", 100*frac)
	}
}

// Spec "Same partition, same shard": the sort key is not part of the routing key.
func TestRoutingKeyIgnoresSortKeyAndSeparatesFields(t *testing.T) {
	if string(RoutingKey("ab", "c")) == string(RoutingKey("a", "bc")) {
		t.Fatal(`("ab","c") and ("a","bc") must differ`)
	}
	// Items that share table + partition key share the routing key, whatever their sort key.
	r := ringOf(DefaultVirtualNodes, "shard-1", "shard-2", "shard-3")
	a, _ := r.Lookup(RoutingKey("orders", "customer-7"))
	b, _ := r.Lookup(RoutingKey("orders", "customer-7"))
	if a != b {
		t.Fatal("same partition mapped to two shards")
	}
}

func TestAddAndRemoveAreIdempotent(t *testing.T) {
	r := ringOf(4, "a", "a", "b")
	if got := len(r.points); got != 8 {
		t.Fatalf("points = %d, want 8 (2 shards x 4)", got)
	}
	r.Remove("missing")
	if got := r.Shards(); len(got) != 2 {
		t.Fatalf("shards = %v", got)
	}
}
