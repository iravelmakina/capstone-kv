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
	// FirstLeaderWait is how long a new set waits for all its replicas before its first
	// leader is chosen from a majority (ADR-0036: 3 minutes; M4 saw nodes 1–2 minutes late).
	FirstLeaderWait time.Duration
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
	client *http.Client // short timeout: asking followers before a promotion
	ctl    *http.Client // no timeout of its own: transfer calls carry their own deadline
}

type replicaSet struct {
	firstSeen    time.Time // when its first replica reported (start of the first-leader wait)
	leader       string
	epoch        uint64
	replicas     map[string]*replicaInfo
	next         atomic.Uint64 // read rotation
	unavailable  atomic.Uint64 // item requests answered 503
	notCommitted atomic.Uint64 // writes answered 504
	// hold is non-nil while the set's writes are held (a planned transfer, ADR-0036);
	// it is closed when they may go on. transferring is set for the transfer's duration.
	hold         chan struct{}
	transferring bool
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
	if cfg.FirstLeaderWait == 0 {
		cfg.FirstLeaderWait = 3 * time.Minute
	}
	m := &Membership{cfg: cfg, shards: map[string]*replicaSet{}, ring: r, now: time.Now,
		client: &http.Client{Timeout: cfg.AskTimeout}, ctl: &http.Client{}}
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
		set = &replicaSet{replicas: map[string]*replicaInfo{}, firstSeen: m.now()}
		m.shards[id] = set
		m.ring.Add(id)
	}
	return set
}

func (m *Membership) liveLocked(info *replicaInfo) bool {
	return info != nil && m.now().Sub(info.lastSeen) <= m.cfg.Timeout
}

// Heartbeat records a replica's report and answers with its set's epoch and leader. A
// set without a leader gets its first one once all its replicas have reported, or once a
// majority has and the set has waited one timeout for the rest.
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
	a := shard.HeartbeatAnswer{Epoch: set.epoch, Leader: set.leader, Transferring: set.transferring}
	if info := set.replicas[set.leader]; info != nil {
		a.LeaderAddress = info.address
	}
	return a
}

// candidate is a replica that could lead: its ID, its last record, and its node.
type candidate struct {
	id   string
	last store.Version
	node string
}

// rankCandidates sorts candidates best first (ADR-0036): the newest last record first,
// which is the safety rule and always wins (ADR-0020); among equals, one on a node that
// leads no other shard; then the preferred replica; then the lowest ID.
func rankCandidates(cands []candidate, busy map[string]int, preferred string) {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.last != b.last {
			return b.last.Less(a.last)
		}
		if aFree, bFree := busy[a.node] == 0, busy[b.node] == 0; aFree != bFree {
			return aFree
		}
		if (a.id == preferred) != (b.id == preferred) {
			return a.id == preferred
		}
		return a.id < b.id
	})
}

// leaderNodesLocked counts, per node, the shards other than except whose leader runs
// there. Callers hold m.mu.
func (m *Membership) leaderNodesLocked(except string) map[string]int {
	busy := map[string]int{}
	for id, set := range m.shards {
		if id == except || set.leader == "" {
			continue
		}
		if info := set.replicas[set.leader]; info != nil {
			busy[nodeOf(info.address)]++
		}
	}
	return busy
}

// assignFirstLeaderLocked picks the first leader of a set from its live replicas, ranked by
// rankCandidates. It waits for all replicas, so the preferred one gets the chance to lead
// and leaders land on different nodes (ADR-0020), but not longer than FirstLeaderWait once
// a majority is there, so a dead replica can't block a start (ADR-0036: a node late by a
// minute or two used to leave both leaders on another node). Callers hold m.mu.
func (m *Membership) assignFirstLeaderLocked(id string, set *replicaSet) error {
	var cands []candidate
	var maxEpoch uint64
	for rid, info := range set.replicas {
		if m.liveLocked(info) {
			cands = append(cands, candidate{rid, info.last, nodeOf(info.address)})
			maxEpoch = max(maxEpoch, info.epoch, info.last.Epoch)
		}
	}
	waited := m.now().Sub(set.firstSeen) > m.cfg.FirstLeaderWait
	if len(cands) < m.cfg.Replicas && (len(cands) <= m.cfg.Replicas/2 || !waited) {
		return nil // wait for the rest, or at least for a majority and FirstLeaderWait
	}
	rankCandidates(cands, m.leaderNodesLocked(id), m.cfg.Preferred[id])
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
		busy      map[string]int    // nodes leading other shards, for the tie-break
	}
	var jobs []job
	m.mu.RLock()
	for id, set := range m.shards {
		if set.leader == "" || set.transferring || m.liveLocked(set.replicas[set.leader]) {
			continue // a set in a planned transfer is the transfer's to settle
		}
		j := job{id: id, epoch: set.epoch, followers: map[string]string{}, busy: m.leaderNodesLocked(id)}
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
		var cands []candidate
		ok := true
		for rid, addr := range j.followers {
			last, err := m.askLast(ctx, addr)
			if err != nil {
				log.Printf("%s: leader is silent, but %s didn't answer (%v): not promoting", j.id, rid, err)
				ok = false
				break
			}
			cands = append(cands, candidate{rid, last, nodeOf(addr)})
		}
		if !ok {
			continue
		}
		// Newest last record first (safety); among equals a node leading nothing else.
		rankCandidates(cands, j.busy, "")
		best := cands[0].id
		m.mu.Lock()
		set := m.shards[j.id]
		if set != nil && set.epoch == j.epoch && !set.transferring && !m.liveLocked(set.replicas[set.leader]) {
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

// Ready reports whether the expected shards are known and each has a leader that has
// confirmed its role (reported the set's epoch in a heartbeat): a leader learns it leads
// from the answer to its heartbeat, and until then it refuses writes. Before that, routing
// would send keys to a ring that's about to change or to a leader that doesn't know it.
func (m *Membership) Ready() (bool, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, set := range m.shards {
		if set.transferring {
			// A planned transfer started with a confirmed live leader; meanwhile writes
			// wait on the hold and reads need no leader (ADR-0036). Its new leader is
			// confirmed by the epoch push before the hold is released.
			continue
		}
		if info := set.replicas[set.leader]; set.leader == "" || info == nil || info.epoch < set.epoch {
			return false, len(m.shards)
		}
	}
	return len(m.shards) >= m.cfg.ExpectedShards, len(m.shards)
}

// Readiness is the rule behind GET /health/ready (M5, spec health-endpoints): Ready's
// conditions, plus a quorum of live replicas in every set (2 of 3, what a read needs with
// R = 2), and the reason when it isn't met. GET /health and routing keep using Ready.
func (m *Membership) Readiness() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.shards) < m.cfg.ExpectedShards {
		return false, fmt.Sprintf("%d of %d shards registered", len(m.shards), m.cfg.ExpectedShards)
	}
	ids := make([]string, 0, len(m.shards))
	for id := range m.shards {
		ids = append(ids, id)
	}
	sort.Strings(ids) // the same reason every time, for the same state
	quorum := m.cfg.Replicas/2 + 1
	for _, id := range ids {
		set := m.shards[id]
		if info := set.replicas[set.leader]; set.leader == "" || info == nil || info.epoch < set.epoch {
			return false, id + ": no confirmed leader"
		}
		live := 0
		for _, info := range set.replicas {
			if m.liveLocked(info) {
				live++
			}
		}
		if live < quorum {
			return false, fmt.Sprintf("%s: %d live replicas, %d needed", id, live, quorum)
		}
	}
	return true, ""
}

// LeaderPlacement reports, for every node the coordinator knows, how many shard leaders run
// there (0 included), and which shards are colocated: their leader shares a node with
// another leader while one of their live followers is on a node that leads nothing. Those
// are the shards a planned transfer could spread out (ADR-0036; the ops agent's input
// through LeadersColocated).
func (m *Membership) LeaderPlacement() (perNode map[string]int, colocated []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	perNode = map[string]int{}
	for _, set := range m.shards {
		for _, info := range set.replicas {
			perNode[nodeOf(info.address)] += 0 // every known node gets a row
		}
	}
	for n, c := range m.leaderNodesLocked("") {
		perNode[n] = c
	}
	for id, set := range m.shards {
		leader := set.replicas[set.leader]
		if leader == nil || perNode[nodeOf(leader.address)] < 2 {
			continue
		}
		for rid, info := range set.replicas {
			if rid != set.leader && m.liveLocked(info) && perNode[nodeOf(info.address)] == 0 {
				colocated = append(colocated, id)
				break
			}
		}
	}
	sort.Strings(colocated)
	return perNode, colocated
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
