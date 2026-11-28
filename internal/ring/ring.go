// Package ring is the consistent hashing ring the coordinator uses to pick a shard for
// each key (ADR-0011).
//
// Every shard is placed on a circle of 2^64 positions many times ("virtual nodes"). A key
// is hashed onto the same circle and belongs to the first shard point at or after it,
// wrapping around at the end. Adding or removing a shard only moves the keys next to its
// points; every other key stays where it was.
package ring

import (
	"sort"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// DefaultVirtualNodes is the number of points per shard chosen in ADR-0011, confirmed by
// the distribution test (docs/m2/ring-distribution.md).
const DefaultVirtualNodes = 128

// point is one virtual node: a position on the circle and the shard it belongs to.
type point struct {
	hash  uint64
	shard string
}

// Ring maps keys to shard IDs. It is safe for concurrent use: lookups take a read lock,
// membership changes (rare) take a write lock and rebuild the sorted point list.
type Ring struct {
	mu     sync.RWMutex
	vnodes int
	shards map[string]struct{}
	points []point // sorted by hash
}

// New returns an empty ring that places each shard at vnodes points.
func New(vnodes int) *Ring {
	if vnodes < 1 {
		vnodes = 1
	}
	return &Ring{vnodes: vnodes, shards: map[string]struct{}{}}
}

// Hash is the ring's hash function: xxHash64 (ADR-0011).
func Hash(b []byte) uint64 { return xxhash.Sum64(b) }

// RoutingKey builds the bytes that decide a record's shard: table name and partition key,
// separated by a zero byte so that ("ab","c") and ("a","bc") differ. The sort key is
// deliberately left out, so all items of one partition land on one shard (ADR-0012).
func RoutingKey(table, partitionKey string) []byte {
	b := make([]byte, 0, len(table)+1+len(partitionKey))
	b = append(b, table...)
	b = append(b, 0)
	return append(b, partitionKey...)
}

// Add places a shard on the ring. Adding a shard that is already there changes nothing.
// Point positions depend only on the shard ID, so a shard that restarts at a new address
// with the same ID gets exactly the same points back.
func (r *Ring) Add(shardID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.shards[shardID]; ok {
		return
	}
	r.shards[shardID] = struct{}{}
	r.rebuild()
}

// Remove takes a shard off the ring; its keys fall to the next points clockwise.
func (r *Ring) Remove(shardID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.shards[shardID]; !ok {
		return
	}
	delete(r.shards, shardID)
	r.rebuild()
}

// rebuild recomputes and sorts all points. Callers hold the write lock.
func (r *Ring) rebuild() {
	pts := make([]point, 0, len(r.shards)*r.vnodes)
	for id := range r.shards {
		for i := 0; i < r.vnodes; i++ {
			// Position of virtual node i of shard id: hash("id#i").
			pts = append(pts, point{hash: Hash([]byte(id + "#" + strconv.Itoa(i))), shard: id})
		}
	}
	sort.Slice(pts, func(a, b int) bool {
		if pts[a].hash != pts[b].hash {
			return pts[a].hash < pts[b].hash
		}
		return pts[a].shard < pts[b].shard // tie-break so the order never depends on map order
	})
	r.points = pts
}

// Lookup returns the shard that owns key, or false if the ring is empty.
func (r *Ring) Lookup(key []byte) (string, bool) {
	h := Hash(key)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.points) == 0 {
		return "", false
	}
	// First point at or after h; past the last point, wrap to the first.
	i := sort.Search(len(r.points), func(i int) bool { return r.points[i].hash >= h })
	if i == len(r.points) {
		i = 0
	}
	return r.points[i].shard, true
}

// VirtualNodes is the number of points each shard has on the ring.
func (r *Ring) VirtualNodes() int { return r.vnodes }

// Of builds a ring with the given shards (a migration's "before" or "after", ADR-0033).
func Of(vnodes int, shards []string) *Ring {
	r := New(vnodes)
	for _, s := range shards {
		r.Add(s)
	}
	return r
}

// Shards returns the shard IDs on the ring, sorted.
func (r *Ring) Shards() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.shards))
	for id := range r.shards {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
