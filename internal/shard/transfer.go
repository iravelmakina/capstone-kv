package shard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/httpx"
)

// The replica's side of a planned leader transfer (ADR-0036). The coordinator runs it:
//
//  1. it holds new writes to the shard;
//  2. POST /internal/v1/stepdown on the leader: the leader stops accepting writes, lets the
//     writes it already appended commit, and answers with its final LSN once the target
//     follower has acknowledged it (or 409 when that doesn't happen in time, and resumes);
//  3. the coordinator persists the next epoch with the target as leader;
//  4. POST /internal/v1/epoch on every replica: the new role, applied at once, instead of
//     waiting up to a heartbeat interval for it;
//  5. it releases the held writes to the new leader.
//
// If the coordinator gives up after step 2, it calls POST /internal/v1/resume. If it dies
// in between, the leader resumes only on a heartbeat answer to a heartbeat it sent after
// stepping down that says it still leads the same epoch and no transfer is running
// (applyAnswer). A step-down that simply expired could leave two leaders: the coordinator
// may already have persisted the next epoch.

// StepDownAnswer is the leader's answer to a step-down: its last record, which the target
// holds (acknowledged) and which has committed.
type StepDownAnswer struct {
	LSN uint64 `json:"lsn"`
}

// writeDone ends a write handler's turn (see write): a step-down waits for them all.
func (r *Replica) writeDone() {
	r.mu.Lock()
	r.writing--
	if r.writing == 0 && r.steppingDown {
		r.broadcastLocked()
	}
	r.mu.Unlock()
}

// stepDown is step 2. Query: epoch (the leader's), target (the follower that will lead),
// wait_ms (how long to wait for it).
func (r *Replica) stepDown(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	epoch, err1 := strconv.ParseUint(q.Get("epoch"), 10, 64)
	waitMs, err2 := strconv.Atoi(q.Get("wait_ms"))
	target := q.Get("target")
	if err1 != nil || err2 != nil || waitMs <= 0 || target == "" || target == r.cfg.Replica {
		httpx.WriteError(w, http.StatusBadRequest, "need epoch, target (another replica) and wait_ms")
		return
	}

	// Exclusive gate: every write that passed its leader check has appended by now, and
	// every later one sees steppingDown. So the final LSN below is final.
	r.gate.Lock()
	r.mu.Lock()
	if !r.isLeaderLocked() || r.epoch != epoch {
		r.mu.Unlock()
		r.gate.Unlock()
		httpx.WriteError(w, http.StatusConflict, "not the leader of that epoch")
		return
	}
	r.steppingDown, r.steppedAt = true, time.Now()
	r.mu.Unlock()
	r.gate.Unlock()
	final, _ := r.st.Last()

	// Wait until the target has acknowledged the final record (so it holds every write
	// this leader accepted), the record has committed, and every write handler has
	// answered its client: in-flight writes finish here, in this epoch, with 200.
	deadline := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		done := r.acked[target] >= final.LSN && r.commit >= final.LSN && r.writing == 0
		stillMe := r.isLeaderLocked() && r.epoch == epoch && r.steppingDown
		ch := r.changed
		r.mu.Unlock()
		switch {
		case done && stillMe:
			httpx.WriteJSON(w, http.StatusOK, StepDownAnswer{LSN: final.LSN})
			return
		case !stillMe:
			httpx.WriteError(w, http.StatusConflict, "leadership changed during the step-down")
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			r.resumeEpoch(epoch) // the target didn't catch up: keep leading, nothing changed
			httpx.WriteError(w, http.StatusConflict, "target "+target+" didn't catch up in time; still leading")
			return
		case <-req.Context().Done():
			// The coordinator gave up waiting; it will call resume (or a heartbeat will).
			return
		}
	}
}

// resume ends a step-down of the given epoch: the coordinator abandoned the transfer.
func (r *Replica) resume(w http.ResponseWriter, req *http.Request) {
	epoch, err := strconv.ParseUint(req.URL.Query().Get("epoch"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "need epoch")
		return
	}
	r.resumeEpoch(epoch)
	w.WriteHeader(http.StatusNoContent)
}

func (r *Replica) resumeEpoch(epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epoch == epoch && r.steppingDown {
		r.steppingDown = false
		r.broadcastLocked()
	}
}

// pushEpoch is step 4: the coordinator's new epoch and leader, applied like a heartbeat
// answer. Body: HeartbeatAnswer. It answers with the epoch the replica now has.
func (r *Replica) pushEpoch(w http.ResponseWriter, req *http.Request) {
	var a HeartbeatAnswer
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&a); err != nil || a.Epoch == 0 || a.Leader == "" {
		httpx.WriteError(w, http.StatusBadRequest, "body must be {epoch, leader, leaderAddress}")
		return
	}
	r.SetRole(a.Epoch, a.Leader, a.LeaderAddress)
	r.mu.Lock()
	epoch := r.epoch
	r.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, map[string]uint64{"epoch": epoch})
}

// applyAnswer takes the role from a heartbeat answer. sentAt is when the heartbeat was
// sent, so the coordinator computed the answer after it. A stepped-down leader resumes
// only on an answer to a heartbeat sent after its step-down began that names it leader of
// the same epoch with no transfer running: then the coordinator (perhaps restarted) never
// persisted a new epoch. An older answer may predate the transfer and proves nothing.
func (r *Replica) applyAnswer(a HeartbeatAnswer, sentAt time.Time) {
	r.SetRole(a.Epoch, a.Leader, a.LeaderAddress)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steppingDown && !a.Transferring && sentAt.After(r.steppedAt) &&
		a.Epoch == r.epoch && a.Leader == r.cfg.Replica {
		r.steppingDown = false
		r.broadcastLocked()
	}
}
