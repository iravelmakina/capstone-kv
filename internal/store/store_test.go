package store

import (
	"os"
	"path/filepath"
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

// Spec "Torn last record": a half-written record is dropped, earlier ones survive, and
// new writes after the restart are readable after another restart.
func TestTornTailIsDroppedAndCutOff(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	s.Put("good", []byte("yes"))
	s.Close()

	// Simulate a crash in the middle of writing the next record.
	partial := encode(opPut, "torn", []byte("never finished"))
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

// A flipped byte inside a record fails its checksum; replay stops there.
func TestCorruptRecordStopsReplay(t *testing.T) {
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
