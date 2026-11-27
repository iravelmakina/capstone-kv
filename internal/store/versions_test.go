package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Spec "Stamp per record": the store's stamps rise with every record, also when the
// physical clock stands still or goes back.
func TestStampsRise(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	var prev uint64
	for i, now := range []uint64{1000, 1000, 999, 1001} {
		rec, _, err := s.WriteVersioned(1, OpPut, "k", []byte("v"), WriteOpts{}, now)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Stamp <= prev {
			t.Fatalf("write %d: stamp %d not above %d", i, rec.Stamp, prev)
		}
		prev = rec.Stamp
	}
	if got := s.Clock(); got != prev {
		t.Fatalf("clock %d, want the last stamp %d", got, prev)
	}
}

// Spec "Stamps rise across a promotion": a follower holding the leader's records stamps
// its own first write above all of them, even with a slower clock.
func TestPromotedFollowerStampsAbove(t *testing.T) {
	leader := mustOpen(t, t.TempDir())
	defer leader.Close()
	follower := mustOpen(t, t.TempDir())
	defer follower.Close()
	old, _, _ := leader.WriteVersioned(1, OpPut, "k", []byte("old"), WriteOpts{}, 5000)
	replicateAll(t, leader, follower)
	rec, _, err := follower.WriteVersioned(2, OpPut, "j", []byte("new"), WriteOpts{}, 10) // clock far behind
	if err != nil {
		t.Fatal(err)
	}
	if rec.Stamp <= old.Stamp {
		t.Fatalf("new leader's stamp %d not above the old leader's %d", rec.Stamp, old.Stamp)
	}
}

// Spec "Late retry loses": a write sent back with its first stamp loses to a newer write
// to the key, and the head names the winning record.
func TestLateRetrySuperseded(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	first, _, _ := s.WriteVersioned(1, OpPut, "k", []byte("A"), WriteOpts{}, 1000)
	newer, _, _ := s.WriteVersioned(1, OpPut, "k", []byte("B"), WriteOpts{}, 1001)
	_, head, err := s.WriteVersioned(1, OpPut, "k", []byte("A"), WriteOpts{Stamp: first.Stamp}, 1002)
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("retry err = %v, want ErrSuperseded", err)
	}
	if head.LSN != newer.LSN || head.Stamp != newer.Stamp {
		t.Fatalf("head = %+v, want the newer record %d/%d", head, newer.LSN, newer.Stamp)
	}
	if last, _ := s.Last(); last.LSN != 2 {
		t.Fatalf("a superseded write was appended: last LSN %d", last.LSN)
	}
	// A client stamp above the head wins, and moves the clock forward (receive rule).
	won, _, err := s.WriteVersioned(1, OpPut, "k", []byte("C"), WriteOpts{Stamp: newer.Stamp + 10}, 1003)
	if err != nil || won.Stamp != newer.Stamp+10 {
		t.Fatalf("newer client stamp: %v, stamp %d", err, won.Stamp)
	}
	if next, _, _ := s.WriteVersioned(1, OpPut, "x", nil, WriteOpts{}, 0); next.Stamp <= won.Stamp {
		t.Fatalf("clock didn't move past the client's stamp: %d <= %d", next.Stamp, won.Stamp)
	}
}

// Spec "Lost update prevented" and "Create only".
func TestConditionalWrites(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	if _, _, err := s.WriteVersioned(1, OpPut, "k", []byte("1"), WriteOpts{IfNoneMatch: true}, 100); err != nil {
		t.Fatalf("create of a new key: %v", err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "k", []byte("2"), WriteOpts{IfNoneMatch: true}, 101); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("create of an existing key: %v, want ErrPrecondition", err)
	}
	head, _ := s.Head("k")
	if _, _, err := s.WriteVersioned(1, OpPut, "k", []byte("3"), WriteOpts{HasIfMatch: true, IfMatch: head.Stamp}, 102); err != nil {
		t.Fatalf("first conditional update: %v", err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "k", []byte("4"), WriteOpts{HasIfMatch: true, IfMatch: head.Stamp}, 103); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second update on the same version: %v, want ErrPrecondition", err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "missing", nil, WriteOpts{HasIfMatch: true, IfMatch: 1}, 104); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("If-Match on a missing key: %v, want ErrPrecondition", err)
	}
	// After a delete, create-only works again.
	h, _ := s.Head("k")
	s.WriteVersioned(1, OpDelete, "k", nil, WriteOpts{HasIfMatch: true, IfMatch: h.Stamp}, 105)
	if _, _, err := s.WriteVersioned(1, OpPut, "k", []byte("5"), WriteOpts{IfNoneMatch: true}, 106); err != nil {
		t.Fatalf("create after a delete: %v", err)
	}
}

// Heads and the clock come back after a restart and after truncation.
func TestHeadsAfterReplayAndTruncate(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	a, _, _ := s.WriteVersioned(1, OpPut, "k", []byte("a"), WriteOpts{}, 100)
	s.Apply(a)
	b, _, _ := s.WriteVersioned(1, OpPut, "k", []byte("b"), WriteOpts{}, 200)
	s.Close()
	s = mustOpen(t, dir)
	defer s.Close()
	if h, _ := s.Head("k"); h.LSN != b.LSN || h.Stamp != b.Stamp {
		t.Fatalf("head after replay = %+v, want %d/%d", h, b.LSN, b.Stamp)
	}
	if e, _ := s.Lookup("k"); e.Stamp != b.Stamp {
		t.Fatalf("entry stamp after replay = %d, want %d", e.Stamp, b.Stamp)
	}
	if err := s.TruncateAfter(1); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Head("k"); h.LSN != a.LSN || h.Stamp != a.Stamp {
		t.Fatalf("head after truncation = %+v, want %d/%d", h, a.LSN, a.Stamp)
	}
	if s.Clock() != a.Stamp {
		t.Fatalf("clock after truncation = %d, want %d", s.Clock(), a.Stamp)
	}
}

// Spec "Stamp per record": a log written in format 1 (M3) is still read, with stamp 0,
// and new records after it are format 2; a follower stores the leader's format-1 bytes
// unchanged, so the hash chain holds.
func TestFormat1StillRead(t *testing.T) {
	dir := t.TempDir()
	r1 := Record{LSN: 1, Op: OpPut, Key: "old", Value: []byte(`{"m3":true}`), format: format1}.encode()
	if err := os.WriteFile(filepath.Join(dir, "data.log"), r1, 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, dir)
	defer s.Close()
	if e, ok := s.Lookup("old"); !ok || e.Stamp != 0 || string(e.Value) != `{"m3":true}` {
		t.Fatalf("format-1 record read as %+v, %v", e, ok)
	}
	rec, _, err := s.WriteVersioned(1, OpPut, "new", []byte("x"), WriteOpts{}, 100)
	if err != nil || rec.Stamp == 0 {
		t.Fatalf("write after a format-1 log: %v, stamp %d", err, rec.Stamp)
	}
	follower := mustOpen(t, t.TempDir())
	defer follower.Close()
	replicateAll(t, s, follower) // fails on a broken chain if the bytes changed
	if v, ok := follower.Get("old"); !ok || string(v) != `{"m3":true}` {
		t.Fatal("follower lost the format-1 record")
	}
}
