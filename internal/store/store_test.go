package store

import (
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

// Spec "Torn last record": the last record fails its checksum (pages written out of order
// during a crash). Nothing follows it, so it is a torn tail: dropped, and the shard starts.
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
		"checksum":     func(b []byte) { b[headerSize+5] ^= 0xFF },               // a byte of the first record's key
		"length field": func(b []byte) { binary.BigEndian.PutUint32(b[0:4], 4) }, // shorter than any record
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
	ff := &faultyFile{File: s.f.(*os.File), failWrite: true}
	s.f = ff

	if err := s.Put("lost", []byte("half written")); err == nil {
		t.Fatal("put reported success after a failed write")
	}
	if _, ok := s.Get("lost"); ok {
		t.Fatal("failed put is visible in memory")
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
