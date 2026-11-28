package shard

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// The replica's side of a key migration (ADR-0033, spec shard-rebalancing). The coordinator
// drives it; every endpoint is answered by the replica set's leader only (409 elsewhere).
//
// On a source:
//   - POST /internal/v1/migrate/commit: the leader's commit point, taken once before the copy;
//   - POST /internal/v1/migrate/state: the current records of keys moving to a target, as
//     of that commit point, a page at a time;
//   - POST /internal/v1/migrate/tail: the committed records after an LSN whose keys move to
//     the target: never a record that hasn't committed, which a new leader could cut away;
//   - POST /internal/v1/migrate/freeze: a replicated freeze record for the moving keys, after
//     which their writes get 409 "moved" (or an unfreeze, if the cutover is abandoned);
//   - POST /internal/v1/migrate/purge: replicated purge records for the keys that moved away
//     (or, on an aborted add's target, for the keys it copied).
//
// On a target:
//   - POST /internal/v1/migrate/apply: copies, applied if newer, replicated like any write.

// RingChange is a migration as replicas see it: the sets on the ring before and after,
// with the ring's virtual nodes. Its JSON is the description in a freeze record.
type RingChange struct {
	ID     string   `json:"id"`
	Before []string `json:"before"`
	After  []string `json:"after"`
	VNodes int      `json:"vnodes"`
}

// rings is a RingChange with its two rings built, cached by ID.
type rings struct{ before, after *ring.Ring }

var ringCache sync.Map // change ID -> *rings

func (c RingChange) rings() *rings {
	if v, ok := ringCache.Load(c.ID); ok {
		return v.(*rings)
	}
	rs := &rings{before: ring.Of(c.VNodes, c.Before), after: ring.Of(c.VNodes, c.After)}
	ringCache.Store(c.ID, rs)
	return rs
}

// Owners returns a storage key's owner before and after the change.
func (c RingChange) Owners(storageKey string) (before, after string) {
	rk := routingKeyOf(storageKey)
	rs := c.rings()
	before, _ = rs.before.Lookup(rk)
	after, _ = rs.after.Lookup(rk)
	return before, after
}

// routingKeyOf is the part of a storage key the ring hashes: table and partition key,
// without the sort key (ADR-0012; StorageKey and ring.RoutingKey).
func routingKeyOf(storageKey string) []byte {
	i := strings.IndexByte(storageKey, 0)
	if i < 0 {
		return []byte(storageKey)
	}
	if j := strings.IndexByte(storageKey[i+1:], 0); j >= 0 {
		return []byte(storageKey[:i+1+j])
	}
	return []byte(storageKey)
}

// movingAway reports whether key leaves shard in the change.
func (c RingChange) movingAway(shard, key string) bool {
	b, a := c.Owners(key)
	return b == shard && a != shard
}

// movingTo reports whether key comes from another set to target in the change.
func (c RingChange) movingTo(target, key string) bool {
	b, a := c.Owners(key)
	return a == target && b != target
}

// frozenCheck is the store's FrozenCheck: a key is frozen by a freeze whose ring change
// moves it away from this replica set.
func (r *Replica) frozenCheck(desc []byte, key string) bool {
	var c RingChange
	if json.Unmarshal(desc, &c) != nil || c.ID == "" {
		return false
	}
	return c.movingAway(r.cfg.Shard, key)
}

// MigrateRequest is the body of every migrate endpoint; each uses the fields it needs.
type MigrateRequest struct {
	Change   RingChange   `json:"change"`
	Target   string       `json:"target,omitempty"`   // state, tail: the set the keys move to
	Upto     uint64       `json:"upto,omitempty"`     // state: the commit point
	After    string       `json:"after,omitempty"`    // state: the last key of the previous page
	From     uint64       `json:"from,omitempty"`     // tail: the LSN already applied
	Limit    int          `json:"limit,omitempty"`    // state: keys; tail: records scanned
	Unfreeze bool         `json:"unfreeze,omitempty"` // freeze: lift it instead
	Which    string       `json:"which,omitempty"`    // purge: "away" (source) or "in" (aborted target)
	Copies   []store.Copy `json:"copies,omitempty"`   // apply
}

// MigrateAnswer is what they answer.
type MigrateAnswer struct {
	Commit  uint64       `json:"commit,omitempty"` // commit: the commit point
	Epoch   uint64       `json:"epoch,omitempty"`
	Copies  []store.Copy `json:"copies,omitempty"` // state, tail
	More    bool         `json:"more,omitempty"`   // state: another page follows
	Next    uint64       `json:"next,omitempty"`   // tail: the LSN scanned up to (<= commit)
	LSN     uint64       `json:"lsn,omitempty"`    // freeze, purge, apply: the last record written
	Applied int          `json:"applied"`          // apply: copies appended; purge: keys purged
}

func (r *Replica) migrateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /internal/v1/migrate/commit", r.migrateCommit)
	mux.HandleFunc("POST /internal/v1/migrate/state", r.migrateState)
	mux.HandleFunc("POST /internal/v1/migrate/tail", r.migrateTail)
	mux.HandleFunc("POST /internal/v1/migrate/freeze", r.migrateFreeze)
	mux.HandleFunc("POST /internal/v1/migrate/purge", r.migratePurge)
	mux.HandleFunc("POST /internal/v1/migrate/apply", r.migrateApply)
}

// readMigrate decodes the body and checks that this replica leads; on failure it has
// answered and returns false.
func (r *Replica) readMigrate(w http.ResponseWriter, req *http.Request) (MigrateRequest, uint64, bool) {
	var m MigrateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<20)).Decode(&m); err != nil ||
		m.Change.ID == "" || m.Change.VNodes <= 0 || len(m.Change.After) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "body must carry the ring change {id, before, after, vnodes}")
		return m, 0, false
	}
	r.mu.Lock()
	leader, epoch := r.isLeaderLocked(), r.epoch
	r.mu.Unlock()
	if !leader {
		httpx.WriteError(w, http.StatusConflict, "not the leader of the current epoch")
		return m, 0, false
	}
	return m, epoch, true
}

func (r *Replica) migrateCommit(w http.ResponseWriter, req *http.Request) {
	if _, _, ok := r.readMigrate(w, req); !ok {
		return
	}
	r.mu.Lock()
	ans := MigrateAnswer{Commit: r.commit, Epoch: r.epoch}
	r.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, ans)
}

// migrateState: a page of the keys moving to Target, as of the commit point Upto. The map
// on a leader holds committed records only (it applies after the commit), and Snapshot
// leaves out keys rewritten after Upto, so nothing here is ahead of the commit point.
func (r *Replica) migrateState(w http.ResponseWriter, req *http.Request) {
	m, _, ok := r.readMigrate(w, req)
	if !ok {
		return
	}
	if m.Target == "" {
		httpx.WriteError(w, http.StatusBadRequest, "state needs target (and upto, the commit point)")
		return
	}
	limit := m.Limit
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	copies, more := r.st.Snapshot(m.After, limit, m.Upto, func(k string) bool { return m.Change.movingTo(m.Target, k) })
	httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{Copies: copies, More: more})
}

// migrateTail: the committed records after From (at most Limit scanned) whose keys move to
// Target, and how far it scanned. It stops at the commit point (ADR-0033, AI review W2).
func (r *Replica) migrateTail(w http.ResponseWriter, req *http.Request) {
	m, _, ok := r.readMigrate(w, req)
	if !ok {
		return
	}
	if m.Target == "" {
		httpx.WriteError(w, http.StatusBadRequest, "tail needs target")
		return
	}
	limit := m.Limit
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	r.mu.Lock()
	commit := r.commit
	r.mu.Unlock()
	if m.From >= commit {
		httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{Next: m.From})
		return
	}
	n := min(uint64(limit), commit-m.From)
	raw, _, err := r.st.ReadAfter(m.From, int(n))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "read log: "+err.Error())
		return
	}
	recs, err := store.DecodeRecords(raw)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "decode log: "+err.Error())
		return
	}
	ans := MigrateAnswer{Next: m.From}
	for _, rec := range recs {
		if rec.LSN > commit {
			break // never ship what hasn't committed
		}
		ans.Next = rec.LSN
		if (rec.Op == store.OpPut || rec.Op == store.OpDelete) && m.Change.movingTo(m.Target, rec.Key) {
			ans.Copies = append(ans.Copies, store.Copy{Key: rec.Key, Value: rec.Value, Deleted: rec.Op == store.OpDelete, Stamp: rec.Stamp})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, ans)
}

// migrateFreeze appends a freeze (or unfreeze) record for the change and answers once it
// has committed, with its LSN: from that record on this set refuses writes to the keys
// moving away, and every write it accepted for them is at a lower LSN.
func (r *Replica) migrateFreeze(w http.ResponseWriter, req *http.Request) {
	m, _, ok := r.readMigrate(w, req)
	if !ok {
		return
	}
	op := store.OpFreeze
	var desc []byte
	if m.Unfreeze {
		op = store.OpUnfreeze
	} else {
		desc, _ = json.Marshal(m.Change)
	}
	r.writeControl(w, req, op, m.Change.ID, desc)
}

// migratePurge drops the keys that moved away (which "away") or, on the target of an
// aborted add, the keys it received (which "in"), in records of at most MaxPurgeKeys.
func (r *Replica) migratePurge(w http.ResponseWriter, req *http.Request) {
	m, _, ok := r.readMigrate(w, req)
	if !ok {
		return
	}
	var match func(string) bool
	switch m.Which {
	case "away":
		match = func(k string) bool { return m.Change.movingAway(r.cfg.Shard, k) }
	case "in":
		match = func(k string) bool { return m.Change.movingTo(r.cfg.Shard, k) }
	default:
		httpx.WriteError(w, http.StatusBadRequest, `purge needs which: "away" or "in"`)
		return
	}
	keys := r.st.KeysWhere(match)
	if len(keys) == 0 {
		httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{})
		return
	}
	var last uint64
	for start := 0; start < len(keys); start += store.MaxPurgeKeys {
		batch, _ := store.EncodeKeys(keys[start:min(start+store.MaxPurgeKeys, len(keys))])
		lsn, status := r.appendControl(req, store.OpPurge, m.Change.ID, batch)
		if status != 0 {
			r.answerControl(w, status)
			return
		}
		last = lsn
	}
	httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{LSN: last, Applied: len(keys)})
}

// migrateApply: copies from a source, appended if newer than what this set holds, in one
// fsync, then replicated and committed like any write (W = 2). Copies of keys that don't
// move to this set in the change are refused, whatever the caller sends.
func (r *Replica) migrateApply(w http.ResponseWriter, req *http.Request) {
	m, _, ok := r.readMigrate(w, req)
	if !ok {
		return
	}
	for _, c := range m.Copies {
		if !m.Change.movingTo(r.cfg.Shard, c.Key) {
			httpx.WriteError(w, http.StatusBadRequest, "a copy of a key that doesn't move to "+r.cfg.Shard)
			return
		}
	}
	epoch, ok := r.beginWrite(w)
	if !ok {
		return
	}
	defer r.writeDone()
	start := time.Now()
	n, lsn, err := r.st.ApplyCopies(epoch, m.Copies)
	r.gate.RUnlock()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "apply copies: "+err.Error())
		return
	}
	if n > 0 {
		if status := r.commitAppended(req, epoch, lsn, start); status != 0 {
			r.answerControl(w, status)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{LSN: lsn, Applied: n})
}

// beginWrite takes the write gate and checks leadership, as a client write does, so a
// step-down never misses a migration record. On success the caller holds the gate (shared)
// and must release it after appending, and call writeDone.
func (r *Replica) beginWrite(w http.ResponseWriter) (uint64, bool) {
	r.gate.RLock()
	r.mu.Lock()
	if !r.isLeaderLocked() || r.steppingDown {
		r.mu.Unlock()
		r.gate.RUnlock()
		httpx.WriteError(w, http.StatusConflict, "not the leader, or stepping down")
		return 0, false
	}
	epoch := r.epoch
	r.writing++
	r.mu.Unlock()
	return epoch, true
}

// appendControl appends one control record through the write gate and waits for its
// commit. It returns the record's LSN, or the status to answer (409, 504, or -1).
func (r *Replica) appendControl(req *http.Request, op byte, id string, value []byte) (uint64, int) {
	r.gate.RLock()
	r.mu.Lock()
	if !r.isLeaderLocked() || r.steppingDown {
		r.mu.Unlock()
		r.gate.RUnlock()
		return 0, http.StatusConflict
	}
	epoch := r.epoch
	r.writing++
	r.mu.Unlock()
	defer r.writeDone()
	start := time.Now()
	rec, err := r.st.WriteControl(epoch, op, id, value)
	r.gate.RUnlock()
	if err != nil {
		return 0, http.StatusInternalServerError
	}
	if status := r.commitAppended(req, epoch, rec.LSN, start); status != 0 {
		return 0, status
	}
	return rec.LSN, 0
}

// writeControl is appendControl answering the request itself.
func (r *Replica) writeControl(w http.ResponseWriter, req *http.Request, op byte, id string, value []byte) {
	lsn, status := r.appendControl(req, op, id, value)
	if status != 0 {
		r.answerControl(w, status)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, MigrateAnswer{LSN: lsn})
}

// commitAppended wakes the followers (or commits at once with W = 1) and waits for lsn to
// commit: 0 when it did, 504 on timeout or a leadership change, -1 when the caller left.
func (r *Replica) commitAppended(req *http.Request, epoch, lsn uint64, start time.Time) int {
	r.mu.Lock()
	if r.cfg.W <= 1 {
		r.advanceCommitLocked(lsn)
	} else {
		r.broadcastLocked()
	}
	r.mu.Unlock()
	return r.awaitCommit(req, epoch, lsn, start)
}

func (r *Replica) answerControl(w http.ResponseWriter, status int) {
	switch status {
	case http.StatusConflict:
		httpx.WriteError(w, status, "not the leader, or stepping down")
	case http.StatusGatewayTimeout:
		httpx.WriteError(w, status, "not committed in time; outcome unknown")
	case -1:
	default:
		httpx.WriteError(w, http.StatusInternalServerError, "write failed")
	}
}
