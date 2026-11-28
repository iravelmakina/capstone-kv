package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prefixFrozen is a FrozenCheck for tests: a freeze's description is a key prefix.
func prefixFrozen(desc []byte, key string) bool { return strings.HasPrefix(key, string(desc)) }

func control(t *testing.T, s *Store, op byte, id string, value []byte) Record {
	t.Helper()
	rec, err := s.WriteControl(1, op, id, value)
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(rec)
	return rec
}

func purgeOf(t *testing.T, keys ...string) []byte {
	t.Helper()
	b, err := EncodeKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Spec "Old shard shrinks": a purge drops the keys from the map without tombstones, and
// stays applied after a restart; other keys stay.
func TestPurgeDropsKeysWithoutTombstones(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	for _, k := range []string{"a", "b", "c"} {
		write(t, s, 1, OpPut, k, "v-"+k)
	}
	write(t, s, 1, OpDelete, "b", "") // b is a tombstone before the purge
	control(t, s, OpPurge, "m1", purgeOf(t, "a", "b"))

	check := func(s *Store) {
		t.Helper()
		if n := s.Len(); n != 1 {
			t.Fatalf("Len = %d, want 1", n)
		}
		for _, k := range []string{"a", "b"} {
			if _, ok := s.Lookup(k); ok {
				t.Fatalf("%s still in the map (a purge leaves no tombstone)", k)
			}
			if _, ok := s.Head(k); ok {
				t.Fatalf("%s still has a head", k)
			}
		}
		if v, ok := s.Get("c"); !ok || string(v) != "v-c" {
			t.Fatalf("c = %q %v", v, ok)
		}
	}
	check(s)
	s.Close()
	s = mustOpen(t, dir)
	defer s.Close()
	check(s)
}

// A key written after the purge record (in LSN order) isn't dropped by it.
func TestPurgeKeepsALaterWrite(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	write(t, s, 1, OpPut, "a", "old")
	purge, err := s.WriteControl(1, OpPurge, "m1", purgeOf(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	later := write(t, s, 1, OpPut, "a", "new") // committed before the purge applies
	s.Apply(purge)
	if v, ok := s.Get("a"); !ok || string(v) != "new" {
		t.Fatalf("a = %q %v after a purge older than its write", v, ok)
	}
	if h, _ := s.Head("a"); h.LSN != later.LSN {
		t.Fatalf("head of a at LSN %d, want %d", h.LSN, later.LSN)
	}
}

// Spec "Freeze at the source": from the freeze's append, writes to its keys get ErrFrozen
// (other keys don't); the freeze survives a restart; an unfreeze lifts it.
func TestFreezeRefusesWritesUntilUnfrozen(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.SetFrozenCheck(prefixFrozen)
	write(t, s, 1, OpPut, "moving/x", "1")

	// Appended, not yet applied: it already counts (log state).
	if _, err := s.WriteControl(1, OpFreeze, "ring-2", []byte("moving/")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "moving/x", []byte("2"), WriteOpts{}, nowMillis()); !errors.Is(err, ErrFrozen) {
		t.Fatalf("write to a frozen key: %v, want ErrFrozen", err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "moving/new", []byte("1"), WriteOpts{}, nowMillis()); !errors.Is(err, ErrFrozen) {
		t.Fatalf("new key in a frozen range: %v, want ErrFrozen", err)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "stay/x", []byte("1"), WriteOpts{}, nowMillis()); err != nil {
		t.Fatalf("write to a key that doesn't move: %v", err)
	}
	if v, _ := s.Get("moving/x"); string(v) != "1" {
		t.Fatalf("moving/x = %q, want the value before the freeze", v)
	}

	s.Close()
	s = mustOpen(t, dir) // a promoted leader rebuilds the freeze from its log
	s.SetFrozenCheck(prefixFrozen)
	if f := s.Frozen(); string(f["ring-2"]) != "moving/" {
		t.Fatalf("Frozen after restart = %v", f)
	}
	if _, _, err := s.WriteVersioned(1, OpPut, "moving/x", []byte("2"), WriteOpts{}, nowMillis()); !errors.Is(err, ErrFrozen) {
		t.Fatalf("after restart: %v, want ErrFrozen", err)
	}

	control(t, s, OpUnfreeze, "ring-2", nil)
	if _, _, err := s.WriteVersioned(1, OpPut, "moving/x", []byte("2"), WriteOpts{}, nowMillis()); err != nil {
		t.Fatalf("after unfreeze: %v", err)
	}
	s.Close()
	s = mustOpen(t, dir)
	defer s.Close()
	if f := s.Frozen(); len(f) != 0 {
		t.Fatalf("Frozen after unfreeze and restart = %v", f)
	}
}

// Spec "Freeze survives a leader change": a follower that replicated the freeze holds it
// too, with the same bytes (the hash chain), and refuses writes once it leads.
func TestFreezeAndPurgeReplicate(t *testing.T) {
	leader, follower := mustOpen(t, t.TempDir()), mustOpen(t, t.TempDir())
	defer leader.Close()
	defer follower.Close()
	follower.SetFrozenCheck(prefixFrozen)
	write(t, leader, 1, OpPut, "moving/a", "1")
	write(t, leader, 1, OpPut, "stay/b", "1")
	control(t, leader, OpFreeze, "ring-2", []byte("moving/"))
	control(t, leader, OpPurge, "ring-2", purgeOf(t, "moving/a"))
	replicateAll(t, leader, follower)

	_, lh := leader.Last()
	_, fh := follower.Last()
	if lh != fh {
		t.Fatal("follower's log differs from the leader's (hash chain)")
	}
	if _, ok := follower.Lookup("moving/a"); ok {
		t.Fatal("purge not applied on the follower")
	}
	if _, _, err := follower.WriteVersioned(2, OpPut, "moving/a", []byte("2"), WriteOpts{}, nowMillis()); !errors.Is(err, ErrFrozen) {
		t.Fatalf("follower as new leader: %v, want ErrFrozen", err)
	}
}

// Only control records use format 3; puts and deletes stay in format 2, so a log without
// migrations is still one an M4 binary reads.
func TestFormat3OnlyForControlRecords(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	write(t, s, 1, OpPut, "a", "1")
	control(t, s, OpFreeze, "ring-2", []byte("x"))
	write(t, s, 1, OpDelete, "a", "")
	s.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "data.log"))
	if err != nil {
		t.Fatal(err)
	}
	var formats []byte
	for len(raw) > 0 {
		formats = append(formats, raw[0])
		raw = raw[prefixSize+int(binary.BigEndian.Uint32(raw[1:5]))+sumSize:]
	}
	if fmt.Sprint(formats) != "[2 3 2]" {
		t.Fatalf("record formats = %v, want [2 3 2]", formats)
	}
}

// A whole record in a format this binary doesn't know is refused (ErrNewerFormat), even as
// the last record; a torn one is still cut off as before.
func TestNewerFormatRefusedTornOneCut(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	write(t, s, 1, OpPut, "a", "1")
	s.Close()
	path := filepath.Join(dir, "data.log")
	good, _ := os.ReadFile(path)

	future := []byte{4, 0, 0, 0, 3, 'x', 'y', 'z'} // format 4, body "xyz"
	future = binary.BigEndian.AppendUint32(future, crc32.Checksum(future, castagnoli))
	os.WriteFile(path, append(append([]byte(nil), good...), future...), 0o644)
	if _, err := Open(dir); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("Open with a format 4 record: %v, want ErrNewerFormat", err)
	}
	after, _ := os.ReadFile(path)
	if len(after) != len(good)+len(future) {
		t.Fatal("the refused log was changed")
	}

	os.WriteFile(path, append(append([]byte(nil), good...), future[:6]...), 0o644) // torn
	s = mustOpen(t, dir)
	defer s.Close()
	if v, ok := s.Get("a"); !ok || string(v) != "1" {
		t.Fatalf("a = %q %v after cutting a torn tail", v, ok)
	}
}

func TestControlRecordChecks(t *testing.T) {
	s := mustOpen(t, t.TempDir())
	defer s.Close()
	keys := make([]string, MaxPurgeKeys+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
	}
	if _, err := s.WriteControl(1, OpPurge, "m", purgeOf(t, keys...)); err == nil {
		t.Fatal("purge of 1,001 keys accepted")
	}
	if _, err := s.WriteControl(1, OpPurge, "m", purgeOf(t, keys[:MaxPurgeKeys]...)); err != nil {
		t.Fatalf("purge of 1,000 keys: %v", err)
	}
	if _, err := s.WriteControl(1, OpPut, "m", nil); err == nil {
		t.Fatal("WriteControl accepted a put")
	}
	if _, err := s.WriteControl(1, OpPurge, "m", []byte{0, 9, 'a'}); err == nil {
		t.Fatal("purge with a broken key list accepted")
	}
	got, err := DecodeKeys(purgeOf(t, "a", "", "ccc"))
	if err != nil || fmt.Sprint(got) != "[a  ccc]" {
		t.Fatalf("DecodeKeys round trip = %q %v", got, err)
	}
}
