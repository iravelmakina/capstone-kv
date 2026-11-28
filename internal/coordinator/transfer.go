package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/shard"
)

// Planned leader transfer (ADR-0036, spec leader-placement): move a shard's leadership to
// a named live follower while the old leader is alive, without losing an acknowledged
// write and without a 5xx. The replica's half is internal/shard/transfer.go.

// TransferBudget is the longest a transfer holds the shard's writes (ADR-0036: 2 s).
const TransferBudget = 2 * time.Second

// StatusError is a membership operation that didn't happen, with the HTTP status that
// says why (a transfer, a removal).
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string { return e.Msg }

// TransferError is the StatusError of a transfer.
type TransferError = StatusError

// Transfer moves shard id's leadership to replica to:
//
//  1. hold new writes to the shard (WaitHold);
//  2. step-down on the old leader: it refuses writes, its in-flight writes commit, and it
//     answers once the target has acknowledged its last record;
//  3. persist the next epoch with the target as leader;
//  4. push that epoch to every live replica of the set and wait for their answers;
//  5. release the held writes, which now go to the new leader.
//
// If step 2 fails (the target can't catch up in time), nothing is persisted, the old
// leader is told to resume, and the error says so. The whole transfer holds writes at most
// TransferBudget.
func (m *Membership) Transfer(ctx context.Context, id, to string) (shard.HeartbeatAnswer, error) {
	m.mu.Lock()
	set := m.shards[id]
	switch {
	case set == nil:
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusNotFound, "shard " + id + " not found"}
	case set.replicas[to] == nil:
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusNotFound, "replica " + to + " not found in " + id}
	case set.leader == to:
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict, to + " already leads " + id}
	case set.transferring:
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict, "a transfer of " + id + " is already running"}
	case set.leader == "" || !m.liveLocked(set.replicas[set.leader]):
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict, id + " has no live leader"}
	case !m.liveLocked(set.replicas[to]):
		m.mu.Unlock()
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict, to + " is not live"}
	}
	epoch, from := set.epoch, set.leader
	fromAddr, toAddr := set.replicas[from].address, set.replicas[to].address
	live := map[string]string{} // replica -> address, for the push
	for rid, info := range set.replicas {
		if m.liveLocked(info) {
			live[rid] = info.address
		}
	}
	set.transferring, set.hold = true, make(chan struct{}) // step 1
	m.mu.Unlock()
	defer func() { // step 5, whatever happened
		m.mu.Lock()
		close(set.hold)
		set.hold, set.transferring = nil, false
		m.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, TransferBudget)
	defer cancel()
	start := time.Now()

	// Step 2. Leave the leader a little of the budget for steps 3 and 4.
	wait := TransferBudget - 300*time.Millisecond
	if err := m.stepDown(ctx, fromAddr, epoch, to, wait); err != nil {
		m.resume(fromAddr, epoch)
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict,
			fmt.Sprintf("transfer of %s abandoned, %s still leads: %v", id, from, err)}
	}

	// Step 3.
	m.mu.Lock()
	if m.shards[id] != set || set.epoch != epoch {
		m.mu.Unlock()
		m.resume(fromAddr, epoch)
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusConflict, "leadership of " + id + " changed during the transfer"}
	}
	if err := m.setLeaderLocked(id, set, to, epoch+1); err != nil {
		m.mu.Unlock()
		m.resume(fromAddr, epoch)
		return shard.HeartbeatAnswer{}, &TransferError{http.StatusInternalServerError, err.Error()}
	}
	m.mu.Unlock()

	// Step 4: every live replica at once; a replica that misses it learns the epoch from
	// its next heartbeat, as before M5.
	ans := shard.HeartbeatAnswer{Epoch: epoch + 1, Leader: to, LeaderAddress: toAddr}
	var wg sync.WaitGroup
	for rid, addr := range live {
		wg.Add(1)
		go func(rid, addr string) {
			defer wg.Done()
			if err := m.pushEpoch(ctx, addr, ans); err != nil {
				log.Printf("%s: epoch %d not pushed to %s: %v (it learns it from its next heartbeat)", id, ans.Epoch, rid, err)
				return
			}
			m.noteEpoch(id, rid, ans.Epoch)
		}(rid, addr)
	}
	wg.Wait()
	log.Printf("%s: leadership transferred %s -> %s, epoch %d, writes held %s", id, from, to, ans.Epoch,
		time.Since(start).Round(time.Millisecond))
	return ans, nil
}

// WaitHold waits while shard id's writes are held, at most TransferBudget plus a margin,
// and returns how long it waited.
func (m *Membership) WaitHold(ctx context.Context, id string) time.Duration {
	m.mu.RLock()
	var hold chan struct{}
	if set := m.shards[id]; set != nil {
		hold = set.hold
	}
	m.mu.RUnlock()
	if hold == nil {
		return 0
	}
	start := time.Now()
	t := time.NewTimer(TransferBudget + time.Second)
	defer t.Stop()
	select {
	case <-hold:
	case <-t.C:
	case <-ctx.Done():
	}
	return time.Since(start)
}

// noteEpoch records that a replica acknowledged an epoch (the push's answer), as a
// heartbeat would: the new leader counts as confirmed at once, so Ready doesn't drop.
func (m *Membership) noteEpoch(id, rid string, epoch uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if set := m.shards[id]; set != nil {
		if info := set.replicas[rid]; info != nil && info.epoch < epoch {
			info.epoch = epoch
		}
	}
}

func (m *Membership) stepDown(ctx context.Context, addr string, epoch uint64, target string, wait time.Duration) error {
	q := url.Values{"epoch": {strconv.FormatUint(epoch, 10)}, "target": {target},
		"wait_ms": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	resp, err := m.post(ctx, addr+"/internal/v1/stepdown?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("step-down: %d %s", resp.StatusCode, errorText(b))
	}
	return nil
}

// resume tells the old leader to go on leading epoch; best effort with its own short
// deadline (the transfer's may be spent). If it is lost, the leader's next heartbeat
// answer resumes it (shard.applyAnswer).
func (m *Membership) resume(addr string, epoch uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := m.post(ctx, addr+"/internal/v1/resume?epoch="+strconv.FormatUint(epoch, 10), nil)
	if err != nil {
		log.Printf("resume %s at epoch %d: %v (its next heartbeat resumes it)", addr, epoch, err)
		return
	}
	resp.Body.Close()
}

func (m *Membership) pushEpoch(ctx context.Context, addr string, a shard.HeartbeatAnswer) error {
	body, _ := json.Marshal(a)
	resp, err := m.post(ctx, addr+"/internal/v1/epoch", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var got struct {
		Epoch uint64 `json:"epoch"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil || got.Epoch < a.Epoch {
		return fmt.Errorf("status %d, epoch %d", resp.StatusCode, got.Epoch)
	}
	return nil
}

func (m *Membership) post(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return m.ctl.Do(req)
}
