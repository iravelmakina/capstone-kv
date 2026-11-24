package coordinator

import (
	"sort"
	"sync"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
)

// Membership tracks which shards exist, where they live and whether they're alive
// (ADR-0013). A shard joins by registering; it stays on the ring when it goes silent, so
// its keys answer 503 instead of being silently remapped to a shard that doesn't have
// their data. Only an explicit Remove takes it off the ring.
type Membership struct {
	mu       sync.RWMutex
	members  map[string]*member
	ring     *ring.Ring
	timeout  time.Duration // silent longer than this = unavailable (3 heartbeats)
	expected int           // shards needed before item requests are served
	now      func() time.Time
}

type member struct {
	address  string
	lastSeen time.Time
}

// ShardStatus is one row of GET /internal/v1/shards.
type ShardStatus struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	Available bool   `json:"available"`
	SilentFor string `json:"silentFor"`
}

// NewMembership returns an empty membership over r.
func NewMembership(r *ring.Ring, timeout time.Duration, expected int) *Membership {
	return &Membership{members: map[string]*member{}, ring: r, timeout: timeout, expected: expected, now: time.Now}
}

// Register records a heartbeat. An unknown ID joins the ring; a known one just updates
// its address (it may have restarted elsewhere) and its last-seen time.
func (m *Membership) Register(id, address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[id]
	if !ok {
		mem = &member{}
		m.members[id] = mem
		m.ring.Add(id)
	}
	mem.address = address
	mem.lastSeen = m.now()
}

// Remove takes a shard off the ring. Its keys move to the next shards clockwise (without
// their data, until M5's migration exists). Not permanent: a shard that is still running
// re-registers with its next heartbeat, so stop it first. Membership is in memory only; a
// persisted ring with explicit join/leave is M5's design (AI review §1.1).
func (m *Membership) Remove(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[id]; !ok {
		return false
	}
	delete(m.members, id)
	m.ring.Remove(id)
	return true
}

// Ready reports whether enough shards have registered since the coordinator started.
// Before that, routing would send keys to a ring that's about to change.
func (m *Membership) Ready() (bool, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.members) >= m.expected, len(m.members)
}

// Owner returns the shard that owns routingKey, its address and whether it's alive.
func (m *Membership) Owner(routingKey []byte) (id, address string, available, ok bool) {
	id, ok = m.ring.Lookup(routingKey)
	if !ok {
		return "", "", false, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem := m.members[id]
	if mem == nil { // removed between the two lookups
		return id, "", false, true
	}
	return id, mem.address, m.now().Sub(mem.lastSeen) <= m.timeout, true
}

// Status lists every shard, sorted by ID.
func (m *Membership) Status() []ShardStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ShardStatus, 0, len(m.members))
	for id, mem := range m.members {
		silent := m.now().Sub(mem.lastSeen)
		out = append(out, ShardStatus{
			ID: id, Address: mem.address,
			Available: silent <= m.timeout,
			SilentFor: silent.Round(time.Second).String(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
