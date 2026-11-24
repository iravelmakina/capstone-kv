// Package store keeps a shard's keys in memory and makes every change durable in an
// append-only log on disk, replayed at start (ADR-0013).
//
// Log format: a sequence of records, each
//
//	[4 bytes length][4 bytes CRC-32C of payload][payload]
//
// where payload is
//
//	[1 byte op: 1=put, 2=delete][4 bytes key length][key][value]
//
// Integers are big-endian. Length and checksum let replay find a damaged record. What
// happens then depends on where it is (ADR-0018):
//
//   - at the end of the file (its header is cut short, or its declared end reaches the
//     end of the file): a crash during an append. Replay keeps everything before it and
//     cuts it off, so new appends never land after garbage;
//   - followed by more bytes: damage in the middle, which a crash can't cause. Open fails
//     and names the offset; a person decides. Acknowledged writes are never cut silently.
//
// A failed write is cut back the same way before Put/Delete return the error.
package store

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	opPut    byte = 1
	opDelete byte = 2

	headerSize = 8       // length + checksum
	maxRecord  = 8 << 20 // 8 MiB: anything longer is treated as a damaged length field
)

// castagnoli is CRC-32C, the checksum variant with hardware support on x86 and ARM.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrDamaged is returned by Open when the log is damaged somewhere other than its last
// record (ADR-0018).
var ErrDamaged = errors.New("log damaged in the middle")

// logFile is what the store needs from the log file; *os.File has it. Tests swap in a
// file that fails on purpose.
type logFile interface {
	io.Writer
	io.Seeker
	io.Closer
	Sync() error
	Truncate(size int64) error
}

// Store is a durable key-value map. Safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	data map[string][]byte
	f    logFile
	end  int64 // offset just after the last good record: where the next append goes
	// broken is set when a failed write couldn't be cut back: from then on every write
	// fails, so nothing is ever appended after garbage. Cleared only by a restart.
	broken error
}

// Open loads (or creates) the log in dir and replays it into memory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "data.log"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{data: map[string][]byte{}, f: f}
	good, err := s.replay(f)
	if err != nil {
		f.Close() // nothing is cut: the file stays as it was for a person to look at
		return nil, err
	}
	s.end = good
	// Drop a torn tail (if any) and position the file for appends.
	if err := s.cutBack(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// replay applies every intact record and returns the offset just after the last one. A
// damaged record that isn't the last thing in the file is an error (ErrDamaged).
func (s *Store) replay(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	r := bufio.NewReader(f)
	var offset int64
	header := make([]byte, headerSize)

	// damaged decides what a bad record at offset means, from where it says it ends.
	damaged := func(n uint32, what string) (int64, error) {
		end := offset + int64(headerSize) + int64(n)
		if end >= size {
			return offset, nil // the last record: torn by a crash, cut off by Open
		}
		return offset, fmt.Errorf("%w: %s: %s at offset %d, %d more bytes follow; "+
			"restore the file, or truncate it at that offset by hand and accept the loss",
			ErrDamaged, f.Name(), what, offset, size-end)
	}

	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return offset, nil // clean end of file, or a header cut short: a torn tail
		}
		n := binary.BigEndian.Uint32(header[0:4])
		sum := binary.BigEndian.Uint32(header[4:8])
		if n < 5 || n > maxRecord {
			return damaged(n, "impossible record length")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return offset, nil // payload cut short: runs past the end, a torn tail
		}
		if crc32.Checksum(payload, castagnoli) != sum {
			return damaged(n, "checksum mismatch")
		}
		if err := s.apply(payload); err != nil {
			return damaged(n, err.Error())
		}
		offset += int64(headerSize) + int64(n)
	}
}

// cutBack makes the file end at the last good record and positions it for the next append.
func (s *Store) cutBack() error {
	if err := s.f.Truncate(s.end); err != nil {
		return err
	}
	if _, err := s.f.Seek(s.end, io.SeekStart); err != nil {
		return err
	}
	return s.f.Sync()
}

// apply decodes one payload and changes the in-memory map.
func (s *Store) apply(p []byte) error {
	op := p[0]
	klen := binary.BigEndian.Uint32(p[1:5])
	if int(klen) > len(p)-5 {
		return errors.New("key length past end of record")
	}
	key := string(p[5 : 5+klen])
	switch op {
	case opPut:
		s.data[key] = p[5+klen:]
	case opDelete:
		delete(s.data, key)
	default:
		return fmt.Errorf("unknown op %d", op)
	}
	return nil
}

// encode builds one complete record (header + payload).
func encode(op byte, key string, value []byte) []byte {
	n := 1 + 4 + len(key) + len(value)
	rec := make([]byte, headerSize+n)
	p := rec[headerSize:]
	p[0] = op
	binary.BigEndian.PutUint32(p[1:5], uint32(len(key)))
	copy(p[5:], key)
	copy(p[5+len(key):], value)
	binary.BigEndian.PutUint32(rec[0:4], uint32(n))
	binary.BigEndian.PutUint32(rec[4:8], crc32.Checksum(p, castagnoli))
	return rec
}

// appendRecord writes a record and waits until the disk has it (fsync). Callers hold the
// write lock, so log order always equals the order changes reach memory.
//
// If the write or the fsync fails, part of the record may already be in the file. It is
// cut back to the last good record before the error is returned; otherwise the next
// successful write would land after garbage and be lost at the next replay (ADR-0018).
func (s *Store) appendRecord(rec []byte) error {
	if s.broken != nil {
		return s.broken
	}
	_, err := s.f.Write(rec)
	if err == nil {
		err = s.f.Sync()
	}
	if err == nil {
		s.end += int64(len(rec))
		return nil
	}
	if cerr := s.cutBack(); cerr != nil {
		s.broken = fmt.Errorf("store stopped accepting writes: a write failed (%v) and the log could not be cut back (%v)", err, cerr)
	}
	return err
}

// Put stores value under key. It returns only after the change is on disk.
func (s *Store) Put(key string, value []byte) error {
	if len(key)+len(value)+5 > maxRecord {
		return errors.New("record too large")
	}
	v := append([]byte(nil), value...) // own copy: the caller may reuse its buffer
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendRecord(encode(opPut, key, v)); err != nil {
		return err
	}
	s.data[key] = v
	return nil
}

// Get returns the value for key and whether it exists.
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// Delete removes key and reports whether it existed. A missing key writes nothing.
func (s *Store) Delete(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return false, nil
	}
	if err := s.appendRecord(encode(opDelete, key, nil)); err != nil {
		return false, err
	}
	delete(s.data, key)
	return true, nil
}

// Keys returns every key (for listing tables; not used on hot paths).
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	return keys
}

// Len returns the number of keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// Close flushes and closes the log.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
