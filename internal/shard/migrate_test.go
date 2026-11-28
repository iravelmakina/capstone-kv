package shard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/store"
)

// The replica's side of a key migration (ADR-0033, spec shard-rebalancing): shard-3 joins
// a ring of shard-1 alone, so about half of shard-1's keys move to it.

var change = RingChange{ID: "add-shard-3-1", Before: []string{"shard-1"}, After: []string{"shard-1", "shard-3"}, VNodes: 128}

// replicaSet starts three replicas of a set (names like "1a") with leader <prefix>a.
func replicaSet(t *testing.T, shardID, prefix string, cfg Config) (leader, b, c *node) {
	t.Helper()
	cfg.Shard = shardID
	leader = startNodeCfg(t, prefix+"a", cfg, true)
	b = startNodeCfg(t, prefix+"b", cfg, true)
	c = startNodeCfg(t, prefix+"c", cfg, true)
	lead(1, leader, leader, b, c)
	return
}

func migrate(t *testing.T, n *node, endpoint string, req MigrateRequest) (int, MigrateAnswer, string) {
	t.Helper()
	body, _ := json.Marshal(req)
	code, out := do(t, "POST", n.srv.URL+"/internal/v1/migrate/"+endpoint, string(body))
	var ans MigrateAnswer
	json.Unmarshal([]byte(out), &ans)
	return code, ans, out
}

func skey(i int) string { return StorageKey("users", fmt.Sprintf("u%d", i), "") }

func get(t *testing.T, n *node, key string) (int, string) {
	t.Helper()
	return do(t, "GET", n.srv.URL+KeyPath(key), "")
}

// Spec "Only moving keys move" and "Write during copy": the copy takes the state as of the
// commit point, page by page, then the committed tail; the target ends with exactly the
// moving keys, at their latest values (updates and a delete after the commit point
// included), and its followers have them too.
func TestCopyStateThenTail(t *testing.T) {
	cfg := testCfg
	cfg.CommitTimeout = 2 * time.Second
	src, _, _ := replicaSet(t, "shard-1", "1", cfg)
	dst, dstB, _ := replicaSet(t, "shard-3", "3", cfg)
	for i := 0; i < 200; i++ {
		if code := put(t, src, skey(i), fmt.Sprintf("v%d", i)); code != 200 {
			t.Fatalf("put %d = %d", i, code)
		}
	}
	_, c, _ := migrate(t, src, "commit", MigrateRequest{Change: change})
	if c.Commit != 200 {
		t.Fatalf("commit point %d, want 200", c.Commit)
	}

	// Writes after the commit point: they must arrive through the tail, not the snapshot.
	moving := []int{}
	for i := 0; i < 200; i++ {
		if change.movingTo("shard-3", skey(i)) {
			moving = append(moving, i)
		}
	}
	put(t, src, skey(moving[0]), "updated")
	do(t, "DELETE", src.srv.URL+KeyPath(skey(moving[1])), "")

	after := ""
	for page := 0; ; page++ {
		_, st, raw := migrate(t, src, "state", MigrateRequest{Change: change, Target: "shard-3", Upto: c.Commit, After: after, Limit: 30})
		if len(st.Copies) == 0 && st.More {
			t.Fatalf("empty page with more: %s", raw)
		}
		if code, ap, raw := migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: st.Copies}); code != 200 || ap.Applied != len(st.Copies) {
			t.Fatalf("apply page %d: %d %s", page, code, raw)
		}
		if !st.More {
			break
		}
		after = st.Copies[len(st.Copies)-1].Key
	}
	_, tail, _ := migrate(t, src, "tail", MigrateRequest{Change: change, Target: "shard-3", From: c.Commit})
	if len(tail.Copies) != 2 || tail.Next != 202 {
		t.Fatalf("tail: %d copies up to %d, want 2 up to 202", len(tail.Copies), tail.Next)
	}
	migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: tail.Copies})

	if code, v := get(t, dst, skey(moving[0])); code != 200 || v != "updated" {
		t.Fatalf("updated key on the target: %d %q", code, v)
	}
	if code, _ := get(t, dst, skey(moving[1])); code != 404 {
		t.Fatalf("deleted key on the target: %d, want 404", code)
	}
	for i := 0; i < 200; i++ {
		code, v := get(t, dst, skey(i))
		switch {
		case !change.movingTo("shard-3", skey(i)):
			if code != 404 {
				t.Fatalf("u%d doesn't move but the target has it", i)
			}
		case i != moving[0] && i != moving[1] && (code != 200 || v != fmt.Sprintf("v%d", i)):
			t.Fatalf("u%d on the target: %d %q", i, code, v)
		}
	}
	want := len(moving) - 1 // one moving key was deleted
	eventually(t, "the target's follower holds the copies", func() bool { return dstB.st.Len() == want })
	if dst.st.Len() != want {
		t.Fatalf("target holds %d keys, want %d", dst.st.Len(), want)
	}
}

// Spec "Uncommitted write on a dying source": a record the source leader has appended but
// not committed is in neither the snapshot nor the tail, so if that leader died and a new
// one cut the record away, the target never had it.
func TestTailNeverShipsUncommitted(t *testing.T) {
	cfg := testCfg
	cfg.Shard, cfg.CommitTimeout = "shard-1", 3*time.Second
	src := startNodeCfg(t, "1a", cfg, true)
	b := startNodeCfg(t, "1b", cfg, false) // no follower pulls: nothing new commits
	c := startNodeCfg(t, "1c", cfg, false)
	lead(1, src, src, b, c)

	k := -1
	for i := 0; k < 0; i++ {
		if change.movingTo("shard-3", skey(i)) {
			k = i
		}
	}
	go func() { // never commits; its error when the test closes the server is expected
		req, _ := http.NewRequest("PUT", src.srv.URL+KeyPath(skey(k)), strings.NewReader("never-committed"))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	eventually(t, "the write appended", func() bool { return lastLSN(src) == 1 })

	_, cp, _ := migrate(t, src, "commit", MigrateRequest{Change: change})
	if cp.Commit != 0 {
		t.Fatalf("commit point %d with nothing committed", cp.Commit)
	}
	_, st, _ := migrate(t, src, "state", MigrateRequest{Change: change, Target: "shard-3", Upto: lastLSN(src)})
	_, tail, _ := migrate(t, src, "tail", MigrateRequest{Change: change, Target: "shard-3", From: 0})
	if len(st.Copies) != 0 || len(tail.Copies) != 0 || tail.Next != 0 {
		t.Fatalf("uncommitted record shipped: state %d copies, tail %d copies up to %d", len(st.Copies), len(tail.Copies), tail.Next)
	}
}

// Spec "Freeze at the source" and "Freeze survives a leader change": after the freeze
// commits, writes to moving keys get 409 "moved", others go on; a promoted follower
// refuses them too; an unfreeze lifts it.
func TestFreezeAnswersMoved(t *testing.T) {
	src, b, c := replicaSet(t, "shard-1", "1", testCfg)
	var moving, staying string
	for i := 0; moving == "" || staying == ""; i++ {
		if change.movingTo("shard-3", skey(i)) {
			moving = skey(i)
		} else {
			staying = skey(i)
		}
	}
	put(t, src, moving, "before")
	code, fr, raw := migrate(t, src, "freeze", MigrateRequest{Change: change})
	if code != 200 || fr.LSN != 2 {
		t.Fatalf("freeze: %d %s", code, raw)
	}
	if code, msg := do(t, "PUT", src.srv.URL+KeyPath(moving), "after"); code != 409 || !strings.Contains(msg, "moved") {
		t.Fatalf("write to a frozen key: %d %s, want 409 moved", code, msg)
	}
	if code := put(t, src, staying, "x"); code != 200 {
		t.Fatalf("write to a key that stays: %d", code)
	}
	eventually(t, "b has the freeze", func() bool { return lastLSN(b) >= 3 })
	push(t, 2, b, src, b, c) // b leads now
	if code, _ := do(t, "PUT", b.srv.URL+KeyPath(moving), "after"); code != 409 {
		t.Fatalf("write to a frozen key on the new leader: %d, want 409", code)
	}
	if code, _, raw := migrate(t, b, "freeze", MigrateRequest{Change: change, Unfreeze: true}); code != 200 {
		t.Fatalf("unfreeze: %d %s", code, raw)
	}
	if code := put(t, b, moving, "after"); code != 200 {
		t.Fatalf("write after the unfreeze: %d", code)
	}
}

// Copies are applied only if newer: applying twice appends nothing new, an older copy
// leaves a newer value alone, and the target's clock moves past copied stamps, so a write
// made on the target afterwards wins.
func TestApplyNewerOnlyAndClockMoves(t *testing.T) {
	dst, _, _ := replicaSet(t, "shard-3", "3", testCfg)
	var key string
	for i := 0; key == ""; i++ {
		if change.movingTo("shard-3", skey(i)) {
			key = skey(i)
		}
	}
	future := uint64(time.Now().Add(time.Hour).UnixMilli()) << 16 // a source clock ahead of ours
	copies := []store.Copy{{Key: key, Value: []byte("copied"), Stamp: future}}
	if _, ap, raw := migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: copies}); ap.Applied != 1 {
		t.Fatalf("first apply: %s", raw)
	}
	if _, ap, _ := migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: copies}); ap.Applied != 0 {
		t.Fatalf("second apply appended %d, want 0", ap.Applied)
	}
	old := []store.Copy{{Key: key, Value: []byte("older"), Stamp: future - 1}}
	if _, ap, _ := migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: old}); ap.Applied != 0 {
		t.Fatal("an older copy was appended")
	}
	if code := put(t, dst, key, "written-here"); code != 200 {
		t.Fatalf("write on the target = %d", code)
	}
	if _, v := get(t, dst, key); v != "written-here" {
		t.Fatalf("value %q: a write on the target after the copy must win (HLC receive rule)", v)
	}
}

// A copy of a key that doesn't move to this set is refused.
func TestApplyRefusesForeignKeys(t *testing.T) {
	dst, _, _ := replicaSet(t, "shard-3", "3", testCfg)
	var key string
	for i := 0; key == ""; i++ {
		if !change.movingTo("shard-3", skey(i)) {
			key = skey(i)
		}
	}
	code, _, _ := migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: []store.Copy{{Key: key, Value: []byte("x"), Stamp: 1 << 20}}})
	if code != 400 {
		t.Fatalf("apply of a key that stays on shard-1: %d, want 400", code)
	}
}

// Spec "Old shard shrinks": purge "away" drops the moving keys from the source set
// (followers included), keeps the others; purge "in" drops what an aborted target got.
func TestPurgeAwayAndIn(t *testing.T) {
	src, srcB, _ := replicaSet(t, "shard-1", "1", testCfg)
	dst, _, _ := replicaSet(t, "shard-3", "3", testCfg)
	var copies []store.Copy
	staying := 0
	for i := 0; i < 100; i++ {
		put(t, src, skey(i), "v")
		if change.movingTo("shard-3", skey(i)) {
			copies = append(copies, store.Copy{Key: skey(i), Value: []byte("v"), Stamp: uint64(i+1) << 16})
		} else {
			staying++
		}
	}
	migrate(t, dst, "apply", MigrateRequest{Change: change, Copies: copies})

	code, pa, raw := migrate(t, src, "purge", MigrateRequest{Change: change, Which: "away"})
	if code != 200 || pa.Applied != len(copies) {
		t.Fatalf("purge away: %d %s", code, raw)
	}
	eventually(t, "the source's follower purged too", func() bool { return srcB.st.Len() == staying })
	if src.st.Len() != staying {
		t.Fatalf("source holds %d keys, want %d", src.st.Len(), staying)
	}
	if code, _, raw := migrate(t, dst, "purge", MigrateRequest{Change: change, Which: "in"}); code != 200 || dst.st.Len() != 0 {
		t.Fatalf("purge in: %d %s; target holds %d", code, raw, dst.st.Len())
	}
}
