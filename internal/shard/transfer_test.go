package shard

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/store"
)

// The replica's side of a planned leader transfer (ADR-0036, spec leader-placement).

// startNodeCfg is startNode with its own settings and, if run is false, no pull loop yet
// (start it with n.run), so a test decides when followers acknowledge.
func startNodeCfg(t *testing.T, id string, cfg Config, run bool) *node {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Replica = id
	n := &node{id: id, st: st, r: NewReplica(st, cfg)}
	n.srv = httptest.NewServer(n.r.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	if run {
		go n.r.Run(ctx)
	}
	t.Cleanup(n.stop)
	return n
}

func stepDown(t *testing.T, n *node, epoch uint64, target string, waitMs int) (int, string) {
	t.Helper()
	return do(t, "POST", n.srv.URL+"/internal/v1/stepdown?epoch="+itoa(epoch)+"&target="+target+"&wait_ms="+itoa(uint64(waitMs)), "")
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

func push(t *testing.T, epoch uint64, leader *node, all ...*node) {
	t.Helper()
	body, _ := json.Marshal(HeartbeatAnswer{Epoch: epoch, Leader: leader.id, LeaderAddress: leader.srv.URL})
	for _, n := range all {
		if code, msg := do(t, "POST", n.srv.URL+"/internal/v1/epoch", string(body)); code != 200 {
			t.Fatalf("push epoch to %s: %d %s", n.id, code, msg)
		}
	}
}

// After a step-down the old leader refuses new writes (409 "stepping down"), and once the
// new epoch is pushed the new leader takes writes at once, without a heartbeat.
func TestStepDownThenNewLeaderServes(t *testing.T) {
	a, b, c := threeNodes(t)
	if code := put(t, a, "k1", "v1"); code != 200 {
		t.Fatalf("put = %d", code)
	}
	code, body := stepDown(t, a, 1, "1b", 2000)
	if code != 200 {
		t.Fatalf("step-down = %d %s", code, body)
	}
	var ans StepDownAnswer
	json.Unmarshal([]byte(body), &ans)
	if ans.LSN != lastLSN(a) {
		t.Fatalf("step-down answered LSN %d, leader's last is %d", ans.LSN, lastLSN(a))
	}
	if code, msg := do(t, "PUT", a.srv.URL+KeyPath("k2"), "v2"); code != 409 || !strings.Contains(msg, "stepping down") {
		t.Fatalf("write after step-down = %d %s, want 409 stepping down", code, msg)
	}
	push(t, 2, b, a, b, c)
	if code := put(t, b, "k2", "v2"); code != 200 {
		t.Fatalf("write to the new leader = %d", code)
	}
	if code, v := do(t, "GET", b.srv.URL+KeyPath("k1"), ""); code != 200 || v != "v1" {
		t.Fatalf("k1 on the new leader = %d %q", code, v)
	}
}

// Spec "Write in flight during step-down": a write already appended when the step-down
// begins is waited for: the step-down answers only after the target holds it and it has
// committed, and the client gets 200. A write that arrives after the step-down began is
// refused, never appended.
func TestStepDownWaitsForAnInFlightWrite(t *testing.T) {
	cfg := testCfg
	cfg.CommitTimeout = 3 * time.Second
	a := startNodeCfg(t, "1a", cfg, true)
	b := startNodeCfg(t, "1b", cfg, false) // not pulling yet: nothing can commit
	c := startNodeCfg(t, "1c", cfg, false)
	lead(1, a, a, b, c)

	inFlight := make(chan int, 1)
	go func() { inFlight <- put(t, a, "k", "in-flight") }()
	eventually(t, "the in-flight write appended", func() bool { return lastLSN(a) == 1 })

	stepped := make(chan [2]any, 1)
	go func() { code, body := stepDown(t, a, 1, "1b", 2500); stepped <- [2]any{code, body} }()
	eventually(t, "the step-down began", func() bool {
		a.r.mu.Lock()
		defer a.r.mu.Unlock()
		return a.r.steppingDown
	})
	if code, msg := do(t, "PUT", a.srv.URL+KeyPath("late"), "x"); code != 409 {
		t.Fatalf("write after the step-down began = %d %s, want 409", code, msg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.r.Run(ctx) // the target catches up and acknowledges LSN 1

	if code := <-inFlight; code != 200 {
		t.Fatalf("in-flight write = %d, want 200", code)
	}
	got := <-stepped
	if got[0].(int) != 200 || !strings.Contains(got[1].(string), `"lsn":1`) {
		t.Fatalf("step-down = %v", got)
	}
	if lastLSN(a) != 1 {
		t.Fatalf("leader's log has %d records; the refused write must not be in it", lastLSN(a))
	}
	push(t, 2, b, a, b, c)
	if code, v := do(t, "GET", b.srv.URL+KeyPath("k"), ""); code != 200 || v != "in-flight" {
		t.Fatalf("the in-flight write on the new leader = %d %q", code, v)
	}
}

// Spec "Target too slow": the target never acknowledges, so the step-down gives up, the
// old leader keeps leading the same epoch, and writes work again.
func TestStepDownAbandonedWhenTargetLags(t *testing.T) {
	a, _, c := threeNodes(t)
	put(t, a, "k1", "v1")
	c.stop()
	put(t, a, "k2", "v2") // c doesn't have it
	if code, msg := stepDown(t, a, 1, "1c", 300); code != 409 || !strings.Contains(msg, "didn't catch up") {
		t.Fatalf("step-down to a lagging target = %d %s, want 409", code, msg)
	}
	if code := put(t, a, "k3", "v3"); code != 200 {
		t.Fatalf("write after an abandoned step-down = %d, want 200", code)
	}
	if s := a.r.Status(); s.Epoch != 1 || s.Role != "leader" {
		t.Fatalf("after abandoning: epoch %d role %s", s.Epoch, s.Role)
	}
}

// If the coordinator dies between the step-down and the epoch push, the old leader
// resumes only on an answer to a heartbeat sent after it stepped down that names it
// leader of the same epoch with no transfer running.
func TestHeartbeatResumesAStrandedStepDown(t *testing.T) {
	a, _, _ := threeNodes(t)
	before := time.Now()
	if code, body := stepDown(t, a, 1, "1b", 2000); code != 200 {
		t.Fatalf("step-down = %d %s", code, body)
	}
	refused := func() bool { code, _ := do(t, "PUT", a.srv.URL+KeyPath("x"), "1"); return code == 409 }

	a.r.applyAnswer(HeartbeatAnswer{Epoch: 1, Leader: "1a"}, before) // sent before the step-down
	if !refused() {
		t.Fatal("resumed on an answer that may predate the transfer")
	}
	a.r.applyAnswer(HeartbeatAnswer{Epoch: 1, Leader: "1a", Transferring: true}, time.Now())
	if !refused() {
		t.Fatal("resumed while the coordinator says a transfer is running")
	}
	a.r.applyAnswer(HeartbeatAnswer{Epoch: 1, Leader: "1a"}, time.Now())
	if refused() {
		t.Fatal("still refusing after the coordinator said it leads, with no transfer running")
	}
}
