package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustOpen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// write is a leader-style write that commits at once (Write + Apply).
func write(t *testing.T, s *Store, epoch uint64, op byte, key, value string) Record {
	t.Helper()
	rec, err := s.Write(epoch, op, key, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(rec)
	return rec
}

// replicateAll copies everything the leader has after the follower's last LSN.
func replicateAll(t *testing.T, leader, follower *Store) {
	t.Helper()
	last, _ := follower.Last()
	raw, _, err := leader.ReadAfter(last.LSN, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Replicate(raw); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetDelete(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	if err := s.Put("k", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Get("k"); !ok || string(v) != `{"a":1}` {
		t.Fatalf("get = %q, %v", v, ok)
	}
	if existed, _ := s.Delete("k"); !existed {
		t.Fatal("delete of an existing key reported false")
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("key still there after delete")
	}
	if existed, _ := s.Delete("k"); existed {
		t.Fatal("delete of a missing key reported true")
	}
	if s.Len() != 0 || len(s.Keys()) != 0 {
		t.Fatal("a tombstone is counted as a key")
	}
}

// Spec "Restart keeps data": puts and deletes survive a reopen.
func TestReopenReplaysLog(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.Put("a", []byte("1"))
	s.Put("b", []byte("2"))
	s.Put("a", []byte("3")) // overwrite
	s.Delete("b")
	s.Close()

	s = mustOpen(t, dir)
	defer s.Close()
	if v, ok := s.Get("a"); !ok || string(v) != "3" {
		t.Fatalf("a = %q, %v; want 3", v, ok)
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("deleted key came back after replay")
	}
}

// Spec "LSNs increase by one": consecutive LSNs, each record linked to the one before.
func TestLSNsAndHashChain(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	for _, k := range []string{"a", "b", "c"} {
		write(t, s, 1, OpPut, k, "v")
	}
	raw, n, err := s.ReadAfter(0, 10)
	if err != nil || n != 3 {
		t.Fatalf("ReadAfter = %d records, %v", n, err)
	}
	recs, err := DecodeRecords(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		if r.LSN != uint64(i+1) {
			t.Fatalf("record %d has LSN %d", i, r.LSN)
		}
		want, _ := s.HashAt(r.LSN - 1)
		if r.Prev != want {
			t.Fatalf("LSN %d: prev hash %x, want %x", r.LSN, r.Prev, want)
		}
	}
	if last, _ := s.Last(); last != (Version{1, 3}) {
		t.Fatalf("last = %+v", last)
	}
}

// ReadAfter honours its limit and starts right after the given LSN.
func TestReadAfterRange(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		write(t, s, 1, OpPut, k, "v")
	}
	raw, n, _ := s.ReadAfter(1, 2)
	recs, _ := DecodeRecords(raw)
	if n != 2 || recs[0].LSN != 2 || recs[1].LSN != 3 {
		t.Fatalf("got %d records starting at %d", n, recs[0].LSN)
	}
	if raw, n, _ := s.ReadAfter(5, 10); n != 0 || raw != nil {
		t.Fatal("records after the last one")
	}
}

// Spec "Delete leaves a tombstone": the key reads as deleted, with its version, also after
// a restart.
func TestTombstoneSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	write(t, s, 1, OpPut, "k", "v")
	del := write(t, s, 1, OpDelete, "k", "")
	s.Close()

	s = mustOpen(t, dir)
	defer s.Close()
	e, ok := s.Lookup("k")
	if !ok || !e.Deleted || e.Version != del.Version() {
		t.Fatalf("lookup = %+v, %v; want a tombstone at %+v", e, ok, del.Version())
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("Get returns a deleted key")
	}
}

// A leader's write is on disk but invisible until it commits (ADR-0023).
func TestWriteIsInvisibleUntilApplied(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	rec, err := s.Write(1, OpPut, "k", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("written but not applied, yet visible")
	}
	s.Apply(rec)
	if v, ok := s.Get("k"); !ok || string(v) != "v" {
		t.Fatal("applied write not visible")
	}
}

// Commits can finish out of order; an older write must not overwrite a newer one.
func TestApplyKeepsNewestVersion(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	old, _ := s.Write(1, OpPut, "k", []byte("old"))
	newer, _ := s.Write(1, OpPut, "k", []byte("new"))
	s.Apply(newer)
	s.Apply(old)
	if v, _ := s.Get("k"); string(v) != "new" {
		t.Fatalf("k = %q, want new", v)
	}
}

func TestStaleEpochRejected(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	write(t, s, 2, OpPut, "k", "v")
	if _, err := s.Write(1, OpPut, "k", []byte("x")); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("write in an older epoch = %v, want ErrStaleEpoch", err)
	}
}

// Spec "Follower stays current": a follower's log ends up byte for byte the leader's.
func TestReplicateMakesTheSameLog(t *testing.T) {
	ldir, fdir := t.TempDir(), t.TempDir()
	leader, follower := mustOpen(t, ldir), mustOpen(t, fdir)
	defer leader.Close()
	defer follower.Close()
	write(t, leader, 1, OpPut, "a", "1")
	write(t, leader, 1, OpPut, "b", "2")
	write(t, leader, 1, OpDelete, "a", "")
	replicateAll(t, leader, follower)

	lb, _ := os.ReadFile(filepath.Join(ldir, "data.log"))
	fb, _ := os.ReadFile(filepath.Join(fdir, "data.log"))
	if !bytes.Equal(lb, fb) {
		t.Fatal("follower's log differs from the leader's")
	}
	if _, ok := follower.Get("a"); ok {
		t.Fatal("follower missed the delete")
	}
	if v, _ := follower.Get("b"); string(v) != "2" {
		t.Fatal("follower missed a put")
	}
}

// Records a follower already has are skipped; a missing LSN is refused.
func TestReplicateSkipsDuplicatesAndRefusesGaps(t *testing.T) {
	leader, follower := mustOpen(t, t.TempDir()), mustOpen(t, t.TempDir())
	defer leader.Close()
	defer follower.Close()
	for _, k := range []string{"a", "b", "c"} {
		write(t, leader, 1, OpPut, k, "v")
	}
	all, _, _ := leader.ReadAfter(0, 10)
	if err := follower.Replicate(all); err != nil {
		t.Fatal(err)
	}
	if err := follower.Replicate(all); err != nil { // the same batch again
		t.Fatalf("duplicates not skipped: %v", err)
	}
	if last, _ := follower.Last(); last.LSN != 3 {
		t.Fatalf("follower at LSN %d after duplicates", last.LSN)
	}
	empty := mustOpen(t, t.TempDir())
	defer empty.Close()
	tail, _, _ := leader.ReadAfter(1, 10) // starts at LSN 2
	if err := empty.Replicate(tail); !errors.Is(err, ErrGap) {
		t.Fatalf("replicate with a gap = %v, want ErrGap", err)
	}
}

// Spec "Broken hash link": two logs that disagree about LSN 2 are caught at LSN 3.
func TestBrokenChainDetected(t *testing.T) {
	leader, follower := mustOpen(t, t.TempDir()), mustOpen(t, t.TempDir())
	defer leader.Close()
	defer follower.Close()
	write(t, leader, 1, OpPut, "a", "1")
	replicateAll(t, leader, follower)
	write(t, leader, 1, OpPut, "b", "leader's")
	write(t, follower, 1, OpPut, "b", "follower's") // a different LSN 2
	write(t, leader, 1, OpPut, "c", "3")

	raw, _, _ := leader.ReadAfter(2, 10)
	if err := follower.Replicate(raw); !errors.Is(err, ErrBrokenChain) {
		t.Fatalf("replicate = %v, want ErrBrokenChain", err)
	}
	if _, ok := follower.Get("c"); ok {
		t.Fatal("a record past the broken link was applied")
	}
}

// Spec "Former leader returns with an uncommitted tail": old leader A wrote LSN 3 in
// epoch 1 that never reached B; B became leader in epoch 2 and wrote its own LSN 3. A cuts
// back to the last LSN of epoch 1 in B's log and catches up.
func TestRejoinTruncatesAndCatchesUp(t *testing.T) {
	a, b := mustOpen(t, t.TempDir()), mustOpen(t, t.TempDir())
	defer a.Close()
	defer b.Close()
	write(t, a, 1, OpPut, "x", "1")
	write(t, a, 1, OpPut, "y", "2")
	replicateAll(t, a, b)
	write(t, a, 1, OpPut, "z", "never committed")
	write(t, b, 2, OpPut, "w", "new leader")

	aLast, aHash := a.Last()
	if h, ok := b.HashAt(aLast.LSN); ok && h == aHash {
		t.Fatal("logs should disagree at A's last record")
	}
	cut := b.LastLSNOfEpoch(aLast.Epoch)
	if cut != 2 {
		t.Fatalf("last LSN of epoch 1 in B = %d, want 2", cut)
	}
	if err := a.TruncateAfter(cut); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get("z"); ok {
		t.Fatal("the uncommitted write survived truncation")
	}
	replicateAll(t, b, a)
	if v, _ := a.Get("w"); string(v) != "new leader" {
		t.Fatal("A didn't catch up after truncation")
	}
	if last, _ := a.Last(); last != (Version{2, 3}) {
		t.Fatalf("A at %+v, want epoch 2 LSN 3", last)
	}
}

// Truncation survives a restart, and the next write lands right after the cut.
func TestTruncateThenWriteThenReopen(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	write(t, s, 1, OpPut, "a", "1")
	write(t, s, 1, OpPut, "b", "2")
	if err := s.TruncateAfter(1); err != nil {
		t.Fatal(err)
	}
	write(t, s, 1, OpPut, "c", "3")
	s.Close()

	s = mustOpen(t, dir)
	defer s.Close()
	if _, ok := s.Get("b"); ok {
		t.Fatal("truncated record came back")
	}
	if v, _ := s.Get("c"); string(v) != "3" {
		t.Fatal("write after truncation lost")
	}
	if last, _ := s.Last(); last.LSN != 2 {
		t.Fatalf("last LSN %d, want 2", last.LSN)
	}
}

// Spec "Torn last record": a half-written record is dropped, earlier ones survive, and
// new writes after the restart are readable after another restart.
func TestTornTailIsDroppedAndCutOff(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.Put("good", []byte("yes"))
	s.Close()

	// Simulate a crash in the middle of writing the next record.
	partial := Record{LSN: 2, Op: OpPut, Key: "torn", Value: []byte("never finished")}.encode()
	f, _ := os.OpenFile(filepath.Join(dir, "data.log"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write(partial[:len(partial)-3])
	f.Close()

	s = mustOpen(t, dir)
	if _, ok := s.Get("torn"); ok {
		t.Fatal("torn record was applied")
	}
	if v, ok := s.Get("good"); !ok || string(v) != "yes" {
		t.Fatal("record before the torn one was lost")
	}
	s.Put("after", []byte("ok")) // must land right after "good", not after garbage
	s.Close()

	s = mustOpen(t, dir)
	defer s.Close()
	if v, ok := s.Get("after"); !ok || string(v) != "ok" {
		t.Fatal("write after recovery was lost: the torn tail wasn't cut off")
	}
}

// Spec "Torn last record": the last record fails its checksum. Nothing follows it, so it is
// a torn tail: dropped, and the shard starts.
func TestDamagedLastRecordIsDropped(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.Put("first", []byte("1"))
	s.Put("second", []byte("2"))
	s.Close()

	path := filepath.Join(dir, "data.log")
	b, _ := os.ReadFile(path)
	b[len(b)-1] ^= 0xFF // damage the last byte of the second record
	os.WriteFile(path, b, 0o644)

	s = mustOpen(t, dir)
	defer s.Close()
	if _, ok := s.Get("first"); !ok {
		t.Fatal("intact first record lost")
	}
	if _, ok := s.Get("second"); ok {
		t.Fatal("corrupt record was applied")
	}
}

// Spec "Damage in the middle": a bad record with more records after it is not a crash.
// Open must fail, name the offset, and leave the file untouched (no silent truncation).
func TestDamageInTheMiddleRefusesToStart(t *testing.T) {
	cases := map[string]func(b []byte){
		"checksum":       func(b []byte) { b[prefixSize+27] ^= 0xFF },                        // a byte of the first key
		"LSN field":      func(b []byte) { b[prefixSize+15] ^= 0xFF },                        // covered by the checksum
		"length field":   func(b []byte) { binary.BigEndian.PutUint32(b[1:5], bodyFixed-1) }, // shorter than any record
		"format version": func(b []byte) { b[0] = 9 },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := mustOpen(t, dir)
			s.Put("first", []byte("1"))
			s.Put("second", []byte("2"))
			s.Put("third", []byte("3"))
			s.Close()

			path := filepath.Join(dir, "data.log")
			b, _ := os.ReadFile(path)
			damage(b)
			os.WriteFile(path, b, 0o644)

			_, err := Open(dir)
			if !errors.Is(err, ErrDamaged) {
				t.Fatalf("Open = %v, want ErrDamaged", err)
			}
			if !strings.Contains(err.Error(), "offset 0") {
				t.Errorf("error doesn't name the offset: %v", err)
			}
			after, _ := os.ReadFile(path)
			if len(after) != len(b) {
				t.Fatalf("log was cut from %d to %d bytes", len(b), len(after))
			}
		})
	}
}

// faultyFile fails the next write after writing only half of it, like a full disk or an
// I/O error part-way through; failTruncate makes cutting back fail too.
type faultyFile struct {
	*os.File
	failWrite, failTruncate bool
}

func (f *faultyFile) Write(p []byte) (int, error) {
	if f.failWrite {
		f.failWrite = false
		n, _ := f.File.Write(p[:len(p)/2])
		return n, errors.New("injected write error")
	}
	return f.File.Write(p)
}

func (f *faultyFile) Truncate(size int64) error {
	if f.failTruncate {
		return errors.New("injected truncate error")
	}
	return f.File.Truncate(size)
}

// Spec "Write error": the failed put returns an error, the half-written bytes are cut off,
// and a later put lands right after the last good record, so it survives a restart.
func TestFailedWriteIsCutBack(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.Put("good", []byte("yes"))
	s.f = &faultyFile{File: s.f.(*os.File), failWrite: true}

	if err := s.Put("lost", []byte("half written")); err == nil {
		t.Fatal("put reported success after a failed write")
	}
	if _, ok := s.Get("lost"); ok {
		t.Fatal("failed put is visible in memory")
	}
	if last, _ := s.Last(); last.LSN != 1 {
		t.Fatalf("failed put took LSN %d", last.LSN)
	}
	if err := s.Put("after", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = mustOpen(t, dir)
	defer s.Close()
	for k, want := range map[string]string{"good": "yes", "after": "ok"} {
		if v, ok := s.Get(k); !ok || string(v) != want {
			t.Fatalf("%s = %q, %v after restart; want %q", k, v, ok, want)
		}
	}
}

// If even cutting back fails, the store refuses all further writes until restarted.
func TestStopsWritingWhenCutBackFails(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	s.f = &faultyFile{File: s.f.(*os.File), failWrite: true, failTruncate: true}

	if err := s.Put("a", []byte("1")); err == nil {
		t.Fatal("put reported success after a failed write")
	}
	if err := s.Put("b", []byte("2")); err == nil || !strings.Contains(err.Error(), "stopped accepting writes") {
		t.Fatalf("second put = %v, want the store to refuse writes", err)
	}
	if _, err := s.Delete("a"); err != nil {
		t.Fatalf("delete of a missing key writes nothing, got %v", err)
	}
}
