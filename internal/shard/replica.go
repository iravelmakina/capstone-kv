package shard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// Headers that carry versions and replication positions between nodes.
const (
	HeaderEpoch      = "X-KV-Epoch"       // a key's version (with LSN), or the leader's epoch
	HeaderLSN        = "X-KV-LSN"         // a key's version (with epoch)
	HeaderLast       = "X-KV-Last"        // the leader's last LSN, on log answers
	HeaderTruncateTo = "X-KV-Truncate-To" // "cut your log back to this LSN" (ADR-0023 step 5)
)

// errNotLeader: the replica asked for the log isn't the current leader.
var errNotLeader = errors.New("not the leader")

// Config is one replica's identity and replication settings (ADR-0021, ADR-0023).
type Config struct {
	Shard         string        // replica set, e.g. "shard-1"
	Replica       string        // stable replica ID, e.g. "1a"
	W             int           // replicas that must have a write before it commits: 1 or 2
	CommitTimeout time.Duration // leader: how long a write waits for a follower (2 s)
	LongPoll      time.Duration // leader: how long a pull is held when there's nothing new (5 s)
	PullDeadline  time.Duration // follower: give up on a pull after this (7 s)
	PullMax       int           // records per pull
}

// Defaults fills in the values the ADRs name.
func (c Config) withDefaults() Config {
	if c.W == 0 {
		c.W = 2
	}
	if c.CommitTimeout == 0 {
		c.CommitTimeout = 2 * time.Second
	}
	if c.LongPoll == 0 {
		c.LongPoll = 5 * time.Second
	}
	if c.PullDeadline == 0 {
		c.PullDeadline = 7 * time.Second
	}
	if c.PullMax == 0 {
		c.PullMax = 1000
	}
	return c
}

// Replica is one member of a shard's replica set: the leader of the current epoch or a
// follower (ADR-0020). The coordinator decides which, through the answers to heartbeats
// (SetRole); a replica never makes itself leader.
type Replica struct {
	cfg  Config
	st   *store.Store
	http *http.Client

	mu         sync.Mutex
	epoch      uint64            // the newest epoch this replica has heard of
	leaderID   string            // the leader of that epoch
	leaderAddr string            // and its base URL
	acked      map[string]uint64 // leader: each follower's durable LSN, from its pulls
	commit     uint64            // leader: highest committed LSN
	applied    uint64            // leader: highest LSN applied to the map
	leaderLast uint64            // follower: the leader's last LSN at the latest pull
	changed    chan struct{}     // closed and replaced on every change (record, ack, role)
	waits      latencies         // leader: commit-wait times

	onPull func(replica string, lsn uint64) // tests: called on every pull the leader serves; guarded by mu
}

// NewReplica wraps an opened store. Until SetRole names a leader it accepts no writes.
func NewReplica(st *store.Store, cfg Config) *Replica {
	last, _ := st.Last()
	return &Replica{
		cfg: cfg.withDefaults(), st: st,
		http:    &http.Client{},
		acked:   map[string]uint64{},
		commit:  last.LSN, // everything replay put in the map counts as applied
		applied: last.LSN,
		changed: make(chan struct{}),
	}
}

// NewStandalone is a replica set of one: leader of epoch 1, writes commit on its own disk.
// It's how a node runs without replication (M2's single shards, and tests).
func NewStandalone(st *store.Store) *Replica {
	r := NewReplica(st, Config{Replica: "standalone", W: 1})
	r.SetRole(1, "standalone", "")
	return r
}

// broadcastLocked wakes everyone waiting for a change. Callers hold r.mu.
func (r *Replica) broadcastLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Replica) isLeaderLocked() bool {
	return r.epoch > 0 && r.leaderID == r.cfg.Replica
}

// SetRole applies what the coordinator says about the replica set: the current epoch and
// its leader. Older epochs are ignored, so a late answer can't undo a promotion.
func (r *Replica) SetRole(epoch uint64, leaderID, leaderAddr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if epoch == 0 || epoch < r.epoch {
		return
	}
	if epoch == r.epoch && leaderID == r.leaderID {
		r.leaderAddr = leaderAddr // the leader may have restarted on a new address
		return
	}
	wasLeader := r.isLeaderLocked()
	r.epoch, r.leaderID, r.leaderAddr = epoch, leaderID, leaderAddr
	r.acked = map[string]uint64{}
	last, _ := r.st.Last()
	switch {
	case r.isLeaderLocked():
		// A promoted follower already applied everything it holds, and all of it is on at
		// least two disks: committed (ADR-0020).
		r.commit, r.applied = last.LSN, last.LSN
	case wasLeader:
		// Demoted: make every record in the log visible. Records the new leader doesn't
		// have are cut off at the first pull, and the map is rebuilt then (ADR-0023).
		r.applyUpToLocked(last.LSN)
	}
	r.broadcastLocked()
}

// applyUpToLocked applies the records after r.applied up to lsn, in order. Callers hold r.mu.
func (r *Replica) applyUpToLocked(lsn uint64) {
	if lsn <= r.applied {
		return
	}
	raw, _, err := r.st.ReadAfter(r.applied, int(lsn-r.applied))
	if err == nil {
		var recs []store.Record
		if recs, err = store.DecodeRecords(raw); err == nil {
			for _, rec := range recs {
				r.st.Apply(rec)
			}
			r.applied = lsn
			return
		}
	}
	log.Printf("replica %s: apply up to LSN %d: %v", r.cfg.Replica, lsn, err)
}

// advanceCommitLocked moves the commit point and makes the newly committed records
// visible. Callers hold r.mu.
func (r *Replica) advanceCommitLocked(lsn uint64) {
	if lsn <= r.commit {
		return
	}
	r.commit = lsn
	r.applyUpToLocked(lsn)
	r.broadcastLocked()
}

// Handler returns the replica's HTTP routes.
func (r *Replica) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", r.health)
	// A "GET" pattern also matches HEAD requests; the handler tells them apart.
	mux.HandleFunc("GET /internal/v1/kv/{key}", r.read)
	mux.HandleFunc("PUT /internal/v1/kv/{key}", func(w http.ResponseWriter, req *http.Request) {
		key, ok := decodeKey(w, req)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxValue))
		if err != nil {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "value larger than 1 MiB")
			return
		}
		r.write(w, req, store.OpPut, key, body, http.StatusOK)
	})
	mux.HandleFunc("DELETE /internal/v1/kv/{key}", func(w http.ResponseWriter, req *http.Request) {
		key, ok := decodeKey(w, req)
		if !ok {
			return
		}
		if _, found := r.st.Get(key); !found {
			w.WriteHeader(http.StatusNotFound) // nothing to delete: nothing written
			return
		}
		r.write(w, req, store.OpDelete, key, nil, http.StatusNoContent)
	})
	mux.HandleFunc("GET /internal/v1/log", r.serveLog)
	return mux
}

// read answers GET and HEAD with the key's value and version. A tombstone answers 404
// with its version, so the coordinator can tell "deleted at (2, 90)" from "never written"
// (no version headers).
func (r *Replica) read(w http.ResponseWriter, req *http.Request) {
	key, ok := decodeKey(w, req)
	if !ok {
		return
	}
	e, found := r.st.Lookup(key)
	if found {
		w.Header().Set(HeaderEpoch, strconv.FormatUint(e.Version.Epoch, 10))
		w.Header().Set(HeaderLSN, strconv.FormatUint(e.Version.LSN, 10))
	}
	if !found || e.Deleted {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK) // EXISTS: status only, no body
		return
	}
	w.Write(e.Value)
}

// write is the leader's write path (ADR-0023 step 3): append and flush, then wait until a
// follower has it (W = 2), then make it visible and answer. A follower answers 409.
func (r *Replica) write(w http.ResponseWriter, req *http.Request, op byte, key string, value []byte, okStatus int) {
	r.mu.Lock()
	if !r.isLeaderLocked() {
		leader := r.leaderID
		r.mu.Unlock()
		httpx.WriteError(w, http.StatusConflict, "not the leader (leader is "+leader+")")
		return
	}
	epoch := r.epoch
	r.mu.Unlock()

	start := time.Now()
	rec, err := r.st.Write(epoch, op, key, value)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	r.mu.Lock()
	if r.cfg.W <= 1 {
		r.advanceCommitLocked(rec.LSN)
	} else {
		r.broadcastLocked() // wake the followers' long-polls: there's a new record
	}
	r.mu.Unlock()

	timeout := time.NewTimer(r.cfg.CommitTimeout)
	defer timeout.Stop()
	for {
		r.mu.Lock()
		committed := r.commit >= rec.LSN && r.epoch == epoch
		stillLeader := r.isLeaderLocked() && r.epoch == epoch
		ch := r.changed
		if committed {
			r.waits.add(time.Since(start))
		}
		r.mu.Unlock()
		switch {
		case committed:
			w.WriteHeader(okStatus)
			return
		case !stillLeader:
			httpx.WriteError(w, http.StatusGatewayTimeout, "not committed: leadership changed; outcome unknown")
			return
		}
		select {
		case <-ch:
		case <-timeout.C:
			r.mu.Lock()
			r.waits.add(time.Since(start))
			r.mu.Unlock()
			httpx.WriteError(w, http.StatusGatewayTimeout, fmt.Sprintf(
				"not committed: no follower had it within %s; outcome unknown", r.cfg.CommitTimeout))
			return
		case <-req.Context().Done():
			return
		}
	}
}

// serveLog is the leader's side of replication (ADR-0023 steps 1, 2, 5). The follower
// sends the epoch, LSN and hash of its last durable record:
//   - if that record isn't in this log, the answer is "cut back to LSN n" (HeaderTruncateTo);
//   - otherwise its LSN is the follower's ack, and the answer is the records after it, or,
//     if there are none yet, whatever arrives within the long-poll time.
func (r *Replica) serveLog(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	follower := q.Get("replica")
	epoch, err1 := strconv.ParseUint(q.Get("epoch"), 10, 64)
	lsn, err2 := strconv.ParseUint(q.Get("lsn"), 10, 64)
	hash, err3 := strconv.ParseUint(q.Get("hash"), 16, 64)
	max, err4 := strconv.Atoi(q.Get("max"))
	if follower == "" || errors.Join(err1, err2, err3, err4) != nil || max <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, "need replica, epoch, lsn, hash (hex) and max")
		return
	}
	r.mu.Lock()
	if !r.isLeaderLocked() {
		r.mu.Unlock()
		httpx.WriteError(w, http.StatusConflict, "not the leader of the current epoch")
		return
	}
	r.mu.Unlock()

	if h, ok := r.st.HashAt(lsn); !ok || h != hash {
		// The follower holds a record this log doesn't: cut back to the end of its epoch
		// here, and always at least one record, so repeated answers make progress.
		target := r.st.LastLSNOfEpoch(epoch)
		if target >= lsn && lsn > 0 {
			target = lsn - 1
		}
		r.writeLogHeaders(w, int64(target))
		w.WriteHeader(http.StatusOK)
		return
	}

	r.mu.Lock()
	if lsn > r.acked[follower] {
		r.acked[follower] = lsn
		if r.cfg.W >= 2 {
			r.advanceCommitLocked(lsn) // one follower is enough for W = 2 (ADR-0021)
		}
	}
	hook := r.onPull
	r.mu.Unlock()
	if hook != nil {
		hook(follower, lsn)
	}

	wait := time.NewTimer(r.cfg.LongPoll)
	defer wait.Stop()
	for {
		r.mu.Lock()
		ch, leader := r.changed, r.isLeaderLocked()
		r.mu.Unlock()
		if !leader {
			httpx.WriteError(w, http.StatusConflict, "not the leader of the current epoch")
			return
		}
		raw, n, err := r.st.ReadAfter(lsn, max)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "read log: "+err.Error())
			return
		}
		if n > 0 {
			r.writeLogHeaders(w, -1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(raw)
			return
		}
		select {
		case <-ch:
		case <-wait.C:
			r.writeLogHeaders(w, -1)
			w.WriteHeader(http.StatusOK) // nothing new: the follower asks again
			return
		case <-req.Context().Done():
			return
		}
	}
}

func (r *Replica) writeLogHeaders(w http.ResponseWriter, truncateTo int64) {
	r.mu.Lock()
	epoch := r.epoch
	r.mu.Unlock()
	last, _ := r.st.Last()
	w.Header().Set(HeaderEpoch, strconv.FormatUint(epoch, 10))
	w.Header().Set(HeaderLast, strconv.FormatUint(last.LSN, 10))
	if truncateTo >= 0 {
		w.Header().Set(HeaderTruncateTo, strconv.FormatInt(truncateTo, 10))
	}
}

// Run pulls the leader's log while this replica is a follower, until ctx ends (ADR-0023).
// After a restart the first pull starts right after the last record on disk.
func (r *Replica) Run(ctx context.Context) {
	for ctx.Err() == nil {
		r.mu.Lock()
		leader, addr := r.isLeaderLocked(), r.leaderAddr
		ch := r.changed
		r.mu.Unlock()
		if leader || addr == "" {
			select { // nothing to pull: wait for a role change
			case <-ch:
			case <-ctx.Done():
			}
			continue
		}
		if err := r.pullOnce(ctx, addr); err != nil && ctx.Err() == nil {
			// 409: the replica we were told leads doesn't know it yet, or no longer does.
			// Leadership is changing, so ask again soon; other errors wait longer.
			wait := 500 * time.Millisecond
			if errors.Is(err, errNotLeader) {
				wait = 50 * time.Millisecond
			} else {
				log.Printf("replica %s: pull from %s: %v", r.cfg.Replica, addr, err)
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
	}
}

// pullOnce asks the leader for the records after this replica's last one and appends them.
func (r *Replica) pullOnce(ctx context.Context, addr string) error {
	last, hash := r.st.Last()
	q := url.Values{
		"replica": {r.cfg.Replica},
		"epoch":   {strconv.FormatUint(last.Epoch, 10)},
		"lsn":     {strconv.FormatUint(last.LSN, 10)},
		"hash":    {strconv.FormatUint(hash, 16)},
		"max":     {strconv.Itoa(r.cfg.PullMax)},
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.PullDeadline)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/internal/v1/log?"+q.Encode(), nil)
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return errNotLeader
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if leaderLast, err := strconv.ParseUint(resp.Header.Get(HeaderLast), 10, 64); err == nil {
		r.mu.Lock()
		r.leaderLast = leaderLast
		r.mu.Unlock()
	}
	if t := resp.Header.Get(HeaderTruncateTo); t != "" {
		to, err := strconv.ParseUint(t, 10, 64)
		if err != nil {
			return fmt.Errorf("bad %s: %q", HeaderTruncateTo, t)
		}
		log.Printf("replica %s: log disagrees with the leader after LSN %d; cutting back", r.cfg.Replica, to)
		return r.st.TruncateAfter(to)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return r.st.Replicate(raw) // checks checksums, gaps and the hash chain
}

// Status is what a replica reports about itself: to the coordinator (promotion reads it)
// and on its health endpoint.
type Status struct {
	Status   string        `json:"status"`
	Role     string        `json:"role"` // leader, follower, or unknown before the first heartbeat
	Version  string        `json:"version"`
	Shard    string        `json:"shard"`
	Replica  string        `json:"replica"`
	Epoch    uint64        `json:"epoch"`
	Leader   string        `json:"leader"`
	Last     store.Version `json:"last"`
	Lag      uint64        `json:"lag"` // follower: records behind the leader at the last pull
	Keys     int           `json:"keys"`
	CommitMs struct {
		Last float64 `json:"last"`
		P99  float64 `json:"p99"`
	} `json:"commitWaitMs"`
}

// Status returns the replica's current state and metrics (ADR-0023).
func (r *Replica) Status() Status {
	last, _ := r.st.Last()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{Status: "ok", Version: buildinfo.Version, Shard: r.cfg.Shard, Replica: r.cfg.Replica,
		Epoch: r.epoch, Leader: r.leaderID, Last: last, Keys: r.st.Len()}
	switch {
	case r.isLeaderLocked():
		s.Role = "leader"
	case r.epoch > 0:
		s.Role = "follower"
		if r.leaderLast > last.LSN {
			s.Lag = r.leaderLast - last.LSN
		}
	default:
		s.Role = "unknown"
	}
	s.CommitMs.Last, s.CommitMs.P99 = r.waits.lastAndP99()
	return s
}

func (r *Replica) health(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, r.Status())
}

// latencies keeps the last 1024 commit waits for a p99 on the health endpoint.
type latencies struct {
	buf  []time.Duration
	next int
}

func (l *latencies) add(d time.Duration) {
	if len(l.buf) < 1024 {
		l.buf = append(l.buf, d)
		return
	}
	l.buf[l.next] = d
	l.next = (l.next + 1) % len(l.buf)
}

func (l *latencies) lastAndP99() (float64, float64) {
	if len(l.buf) == 0 {
		return 0, 0
	}
	lastIdx := len(l.buf) - 1
	if len(l.buf) == 1024 {
		lastIdx = (l.next + len(l.buf) - 1) % len(l.buf)
	}
	sorted := append([]time.Duration(nil), l.buf...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p99 := sorted[(len(sorted)*99)/100]
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return ms(l.buf[lastIdx]), ms(p99)
}

// Heartbeat is what a replica tells the coordinator every few seconds (ADR-0020).
type Heartbeat struct {
	Shard   string        `json:"shard"`
	Replica string        `json:"replica"`
	Address string        `json:"address"` // base URL the coordinator and other replicas call
	Epoch   uint64        `json:"epoch"`   // the newest epoch this replica has heard of
	Last    store.Version `json:"last"`    // its last durable record
}

// HeartbeatAnswer is the coordinator's reply: the replica set's epoch and leader.
type HeartbeatAnswer struct {
	Epoch         uint64 `json:"epoch"`
	Leader        string `json:"leader"`
	LeaderAddress string `json:"leaderAddress"`
}

// HeartbeatLoop reports to the coordinator now and then every interval, until ctx ends,
// and takes its role from each answer. Failures are logged and retried on the next tick:
// the coordinator may start later than the replica, or restart.
func (r *Replica) HeartbeatLoop(ctx context.Context, coordinatorURL, address string, interval time.Duration) {
	client := &http.Client{Timeout: 2 * time.Second}
	send := func() {
		last, _ := r.st.Last()
		r.mu.Lock()
		hb := Heartbeat{Shard: r.cfg.Shard, Replica: r.cfg.Replica, Address: address, Epoch: r.epoch, Last: last}
		r.mu.Unlock()
		body, _ := json.Marshal(hb)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, coordinatorURL+"/internal/v1/replicas", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("heartbeat to %s: %v", coordinatorURL, err)
			return
		}
		defer resp.Body.Close()
		var a HeartbeatAnswer
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&a) != nil {
			log.Printf("heartbeat to %s: status %d", coordinatorURL, resp.StatusCode)
			return
		}
		r.SetRole(a.Epoch, a.Leader, a.LeaderAddress)
	}
	send()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}
