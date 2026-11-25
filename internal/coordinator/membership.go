package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// MembershipConfig holds what the coordinator knows about the deployment.
type MembershipConfig struct {
	Timeout        time.Duration     // silent longer than this = unavailable (3 heartbeats, 15 s)
	ExpectedShards int               // replica sets needed before item requests are served
	Replicas       int               // replicas per shard, N (3)
	Leaders        *store.Store      // persisted leader and epoch per shard (ADR-0020)
	Preferred      map[string]string // shard -> replica to make the first leader, if it can be
	AskTimeout     time.Duration     // per follower, when asking before a promotion (1 s)
}

// Membership tracks every replica set: its replicas, whether each is alive, and its leader
// and epoch (ADR-0020). Shards join the ring when their first replica reports in and stay
// on it; only an explicit Remove takes a shard off.
type Membership struct {
	cfg    MembershipConfig
	mu     sync.RWMutex
	shards map[string]*replicaSet
	ring   *ring.Ring
	now    func() time.Time
	client *http.Client
}

type replicaSet struct {
	leader       string
	epoch        uint64
	replicas     map[string]*replicaInfo
	next         atomic.Uint64 // read rotation
	unavailable  atomic.Uint64 // item requests answered 503
	notCommitted atomic.Uint64 // writes answered 504
}

type replicaInfo struct {
	address  string
	lastSeen time.Time
	epoch    uint64
	last     store.Version
}

// assignment is what the coordinator persists per shard.
type assignment struct {
	Leader string `json:"leader"`
	Epoch  uint64 `json:"epoch"`
}

// NewMembership loads the persisted leaders and returns the membership over r.
func NewMembership(r *ring.Ring, cfg MembershipConfig) (*Membership, error) {
	if cfg.Replicas == 0 {
		cfg.Replicas = 3
	}
	if cfg.AskTimeout == 0 {
		cfg.AskTimeout = time.Second
	}
	m := &Membership{cfg: cfg, shards: map[string]*replicaSet{}, ring: r, now: time.Now,
		client: &http.Client{Timeout: cfg.AskTimeout}}
	for _, id := range cfg.Leaders.Keys() {
		raw, _ := cfg.Leaders.Get(id)
		var a assignment
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("leader of %s: %w", id, err)
		}
		set := m.setLocked(id)
		set.leader, set.epoch = a.Leader, a.Epoch
	}
	return m, nil
}

// setLocked returns the replica set, creating it (and putting it on the ring) if new.
func (m *Membership) setLocked(id string) *replicaSet {
	set, ok := m.shards[id]
	if !ok {
		set = &replicaSet{replicas: map[string]*replicaInfo{}}
		m.shards[id] = set
		m.ring.Add(id)
	}
	return set
}

func (m *Membership) liveLocked(info *replicaInfo) bool {
	return info != nil && m.now().Sub(info.lastSeen) <= m.cfg.Timeout
}

// Heartbeat records a replica's report and answers with its set's epoch and leader. A
// set without a leader gets its first one once a majority of its replicas has reported.
func (m *Membership) Heartbeat(hb shard.Heartbeat) (shard.HeartbeatAnswer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.setLocked(hb.Shard)
	set.replicas[hb.Replica] = &replicaInfo{address: hb.Address, lastSeen: m.now(), epoch: hb.Epoch, last: hb.Last}
	if set.leader == "" {
		if err := m.assignFirstLeaderLocked(hb.Shard, set); err != nil {
			return shard.HeartbeatAnswer{}, err
		}
	}
	return m.answerLocked(set), nil
}

func (m *Membership) answerLocked(set *replicaSet) shard.HeartbeatAnswer {
	a := shard.HeartbeatAnswer{Epoch: set.epoch, Leader: set.leader}
	if info := set.replicas[set.leader]; info != nil {
		a.LeaderAddress = info.address
	}
	return a
}

// assignFirstLeaderLocked picks the first leader of a set: among the live replicas, once
// they are a majority, the one with the newest last record (it has everything any other
// has); on a tie, the preferred replica, then the lowest ID. Callers hold m.mu.
func (m *Membership) assignFirstLeaderLocked(id string, set *replicaSet) error {
	type cand struct {
		id   string
		last store.Version
	}
	var cands []cand
	var maxEpoch uint64
	for rid, info := range set.replicas {
		if m.liveLocked(info) {
			cands = append(cands, cand{rid, info.last})
			maxEpoch = max(maxEpoch, info.epoch, info.last.Epoch)
		}
	}
	if len(cands) <= m.cfg.Replicas/2 {
		return nil // not a majority yet
	}
	pref := m.cfg.Preferred[id]
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.last != b.last {
			return b.last.Less(a.last)
		}
		if (a.id == pref) != (b.id == pref) {
			return a.id == pref
		}
		return a.id < b.id
	})
	return m.setLeaderLocked(id, set, cands[0].id, maxEpoch+1)
}

// setLeaderLocked saves the new leader and epoch to disk, then applies them.
func (m *Membership) setLeaderLocked(id string, set *replicaSet, leader string, epoch uint64) error {
	raw, _ := json.Marshal(assignment{Leader: leader, Epoch: epoch})
	if err := m.cfg.Leaders.Put(id, raw); err != nil {
		return fmt.Errorf("save leader of %s: %w", id, err)
	}
	set.leader, set.epoch = leader, epoch
	log.Printf("%s: leader %s, epoch %d", id, leader, epoch)
	return nil
}

// Watch checks for dead leaders every interval until ctx ends.
func (m *Membership) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.CheckLeaders(ctx)
		}
	}
}

// CheckLeaders promotes a follower in every set whose leader has gone silent (ADR-0020):
// it asks every other replica for its last record, and only if all of them answer does it
// promote the one with the newest, under the next epoch. With W = 2 only all followers
// together are sure to hold every committed write.
func (m *Membership) CheckLeaders(ctx context.Context) {
	type job struct {
		id        string
		epoch     uint64
		followers map[string]string // replica -> address
	}
	var jobs []job
	m.mu.RLock()
	for id, set := range m.shards {
		if set.leader == "" || m.liveLocked(set.replicas[set.leader]) {
			continue
		}
		j := job{id: id, epoch: set.epoch, followers: map[string]string{}}
		for rid, info := range set.replicas {
			if rid != set.leader && m.liveLocked(info) {
				j.followers[rid] = info.address
			}
		}
		if len(j.followers) == m.cfg.Replicas-1 {
			jobs = append(jobs, j)
		}
	}
	m.mu.RUnlock()

	for _, j := range jobs {
		best, bestLast, ok := "", store.Version{}, true
		for rid, addr := range j.followers {
			last, err := m.askLast(ctx, addr)
			if err != nil {
				log.Printf("%s: leader is silent, but %s didn't answer (%v): not promoting", j.id, rid, err)
				ok = false
				break
			}
			if best == "" || bestLast.Less(last) || (last == bestLast && rid < best) {
				best, bestLast = rid, last
			}
		}
		if !ok {
			continue
		}
		m.mu.Lock()
		set := m.shards[j.id]
		if set != nil && set.epoch == j.epoch && !m.liveLocked(set.replicas[set.leader]) {
			if err := m.setLeaderLocked(j.id, set, best, j.epoch+1); err != nil {
				log.Print(err)
			}
		}
		m.mu.Unlock()
	}
}

// askLast reads a replica's last record from its health endpoint.
func (m *Membership) askLast(ctx context.Context, addr string) (store.Version, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/health", nil)
	resp, err := m.client.Do(req)
	if err != nil {
		return store.Version{}, err
	}
	defer resp.Body.Close()
	var s shard.Status
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&s) != nil {
		return store.Version{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	return s.Last, nil
}

// Remove takes a shard off the ring and forgets its leader. Its keys move to the next
// shards clockwise, without their data. Not permanent: a replica that is still running
// reports in again, so stop the set first.
func (m *Membership) Remove(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shards[id]; !ok {
		return false
	}
	m.cfg.Leaders.Delete(id)
	delete(m.shards, id)
	m.ring.Remove(id)
	return true
}

// Ready reports whether the expected shards are known and each has a leader. Before that,
// routing would send keys to a ring that's about to change.
func (m *Membership) Ready() (bool, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, set := range m.shards {
		if set.leader == "" {
			return false, len(m.shards)
		}
	}
	return len(m.shards) >= m.cfg.ExpectedShards, len(m.shards)
}

// WriteTarget returns the shard owning routingKey and its leader's address, if the leader
// is alive.
func (m *Membership) WriteTarget(routingKey []byte) (id, addr string, ok bool) {
	id, found := m.ring.Lookup(routingKey)
	if !found {
		return "", "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := m.shards[id]
	if set == nil || set.leader == "" {
		return id, "", false
	}
	info := set.replicas[set.leader]
	if !m.liveLocked(info) {
		return id, "", false
	}
	return id, info.address, true
}

// ReadTargets returns the shard owning routingKey and its live replicas' addresses, in
// rotating order so that taking the first R spreads reads evenly over all of them.
func (m *Membership) ReadTargets(routingKey []byte) (id string, addrs []string) {
	id, found := m.ring.Lookup(routingKey)
	if !found {
		return "", nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := m.shards[id]
	if set == nil {
		return id, nil
	}
	ids := make([]string, 0, len(set.replicas))
	for rid, info := range set.replicas {
		if m.liveLocked(info) {
			ids = append(ids, rid)
		}
	}
	if len(ids) == 0 {
		return id, nil
	}
	sort.Strings(ids)
	start := int(set.next.Add(1) % uint64(len(ids)))
	for i := range ids {
		addrs = append(addrs, set.replicas[ids[(start+i)%len(ids)]].address)
	}
	return id, addrs
}

// count records a 503 or 504 answered for a shard (ADR-0023 metrics).
func (m *Membership) count(id string, status int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if set := m.shards[id]; set != nil {
		switch status {
		case http.StatusServiceUnavailable:
			set.unavailable.Add(1)
		case http.StatusGatewayTimeout:
			set.notCommitted.Add(1)
		}
	}
}

// ShardStatus is one row of GET /internal/v1/shards.
type ShardStatus struct {
	ID           string          `json:"id"`
	Epoch        uint64          `json:"epoch"`
	Leader       string          `json:"leader"`
	Replicas     []ReplicaStatus `json:"replicas"`
	Unavailable  uint64          `json:"unavailable503"`
	NotCommitted uint64          `json:"notCommitted504"`
}

// ReplicaStatus is one replica in a ShardStatus.
type ReplicaStatus struct {
	ID        string        `json:"id"`
	Address   string        `json:"address"`
	Available bool          `json:"available"`
	SilentFor string        `json:"silentFor"`
	Last      store.Version `json:"last"`
}

// Status lists every shard and its replicas, sorted by ID.
func (m *Membership) Status() []ShardStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ShardStatus, 0, len(m.shards))
	for id, set := range m.shards {
		s := ShardStatus{ID: id, Epoch: set.epoch, Leader: set.leader, Replicas: []ReplicaStatus{},
			Unavailable: set.unavailable.Load(), NotCommitted: set.notCommitted.Load()}
		for rid, info := range set.replicas {
			silent := m.now().Sub(info.lastSeen)
			s.Replicas = append(s.Replicas, ReplicaStatus{ID: rid, Address: info.address,
				Available: m.liveLocked(info), SilentFor: silent.Round(time.Second).String(), Last: info.last})
		}
		sort.Slice(s.Replicas, func(i, j int) bool { return s.Replicas[i].ID < s.Replicas[j].ID })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
