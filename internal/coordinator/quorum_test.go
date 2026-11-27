package coordinator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// fakeReplica answers every read with a fixed status, body and version, and counts reads.
type fakeReplica struct {
	status  int
	body    string
	version store.Version // zero = no version headers (never had the key)
	reads   atomic.Int64
	lastID  atomic.Value // the X-Request-ID of the last request
}

func (f *fakeReplica) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.reads.Add(1)
	f.lastID.Store(r.Header.Get("X-Request-ID"))
	if f.version != (store.Version{}) {
		w.Header().Set(shard.HeaderEpoch, strconv.FormatUint(f.version.Epoch, 10))
		w.Header().Set(shard.HeaderLSN, strconv.FormatUint(f.version.LSN, 10))
	}
	w.WriteHeader(f.status)
	if r.Method == http.MethodGet {
		w.Write([]byte(f.body))
	}
}

// fakeShard is a coordinator with one shard whose 3 replicas are fakes.
func fakeShard(t *testing.T, fakes [3]*fakeReplica) string {
	t.Helper()
	members, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: 1, Replicas: 3, Leaders: openStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	addrs := make([]string, len(fakes))
	for i, f := range fakes {
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		addrs[i] = srv.URL
	}
	for round := uint64(0); round < 2; round++ { // the second round confirms epoch 1
		for i, addr := range addrs {
			if _, err := members.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: string(rune('a' + i)), Address: addr, Epoch: round}); err != nil {
				t.Fatal(err)
			}
		}
	}
	tables := openStore(t)
	tables.Put("users", []byte(`{"name":"users","partitionKey":"userId"}`))
	coord := httptest.NewServer(New(tables, members, 2).Handler())
	t.Cleanup(coord.Close)
	return coord.URL + "/v1/tables/users/items/u1"
}

// Spec "Newest version wins": a tombstone at (3, 90) beats a value at (3, 40).
func TestNewestVersionWins(t *testing.T) {
	v1 := &fakeReplica{status: 200, body: `{"old":true}`, version: store.Version{Epoch: 3, LSN: 40}}
	tomb := &fakeReplica{status: 404, version: store.Version{Epoch: 3, LSN: 90}}
	v2 := &fakeReplica{status: 200, body: `{"old":true}`, version: store.Version{Epoch: 3, LSN: 40}}
	url := fakeShard(t, [3]*fakeReplica{v1, tomb, v2})
	sawPairWithTombstone := false
	for i := 0; i < 6; i++ { // every rotation: two of the three pairs include the tombstone
		before := tomb.reads.Load()
		code, body := call(t, "GET", url, "")
		withTombstone := tomb.reads.Load() > before
		sawPairWithTombstone = sawPairWithTombstone || withTombstone
		if withTombstone && code != 404 {
			t.Fatalf("read %d = %d %q, want 404 from the newer tombstone", i, code, body)
		}
	}
	if !sawPairWithTombstone {
		t.Fatal("no read asked the replica with the tombstone")
	}
}

// Spec "Newer epoch wins": (3, 101) beats (2, 120).
func TestNewerEpochWins(t *testing.T) {
	older := &fakeReplica{status: 200, body: `{"from":"epoch 2"}`, version: store.Version{Epoch: 2, LSN: 120}}
	newer := &fakeReplica{status: 200, body: `{"from":"epoch 3"}`, version: store.Version{Epoch: 3, LSN: 101}}
	url := fakeShard(t, [3]*fakeReplica{older, newer, newer})
	for i := 0; i < 6; i++ {
		if code, body := call(t, "GET", url, ""); code != 200 || body != `{"from":"epoch 3"}` {
			t.Fatalf("read %d = %d %q, want the epoch-3 value", i, code, body)
		}
	}
}

// Spec "Balanced reads": with R = 2, each of 3 replicas serves about 2/3 of the reads.
func TestReadsAreBalanced(t *testing.T) {
	var fakes [3]*fakeReplica
	for i := range fakes {
		fakes[i] = &fakeReplica{status: 200, body: `{}`, version: store.Version{Epoch: 1, LSN: 1}}
	}
	url := fakeShard(t, fakes)
	for i := 0; i < 3000; i++ {
		if code, _ := call(t, "GET", url, ""); code != 200 {
			t.Fatalf("read %d = %d", i, code)
		}
	}
	for i, f := range fakes {
		if n := f.reads.Load(); n < 1800 || n > 2200 {
			t.Fatalf("replica %d served %d of 6,000 replica reads, want 1,800-2,200", i, n)
		}
	}
}

// The first leader of a set is the replica with the newest last record; on a tie, the
// preferred one.
func TestFirstLeaderChoice(t *testing.T) {
	m, _ := NewMembership(ring.New(8), MembershipConfig{Timeout: time.Minute, ExpectedShards: 1,
		Replicas: 3, Leaders: openStore(t), Preferred: map[string]string{"shard-1": "1b"}})
	m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1c", Address: "http://c"})
	a, _ := m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1a", Address: "http://a"})
	if a.Leader != "" {
		t.Fatalf("leader %s chosen before every replica reported or the timeout passed", a.Leader)
	}
	a, _ = m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1b", Address: "http://b"})
	if a.Leader != "1b" || a.Epoch != 1 {
		t.Fatalf("fresh set: leader %s epoch %d, want the preferred 1b in epoch 1", a.Leader, a.Epoch)
	}

	m, _ = NewMembership(ring.New(8), MembershipConfig{Timeout: time.Minute, ExpectedShards: 1,
		Replicas: 3, Leaders: openStore(t), Preferred: map[string]string{"shard-1": "1b"}})
	m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1a", Address: "http://a", Epoch: 4, Last: store.Version{Epoch: 4, LSN: 9}})
	m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1c", Address: "http://c", Epoch: 4, Last: store.Version{Epoch: 4, LSN: 9}})
	a, _ = m.Heartbeat(shard.Heartbeat{Shard: "shard-1", Replica: "1b", Address: "http://b", Epoch: 4, Last: store.Version{Epoch: 4, LSN: 7}})
	if a.Leader != "1a" || a.Epoch != 5 {
		t.Fatalf("set with data: leader %s epoch %d, want the newer 1a in epoch 5", a.Leader, a.Epoch)
	}
}

// Watch promotes on its own once the leader is silent.
func TestWatchPromotes(t *testing.T) {
	c := newCluster(t, 1, true)
	old := c.leaderOf(t, "shard-1")
	old.stop()
	c.later(20 * time.Second)
	c.beat(t, c.others(old.id)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.members.Watch(ctx, 20*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for c.members.Status()[0].Epoch != 2 {
		if time.Now().After(deadline) {
			t.Fatal("Watch didn't promote")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
