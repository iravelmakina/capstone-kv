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
// Integers are big-endian. Length and checksum let replay detect a record that was only
// half written when the machine died: replay stops there and cuts the file back to the
// last good record, so new appends never land after garbage.
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

// Store is a durable key-value map. Safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	data map[string][]byte
	f    *os.File
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
	good, err := s.replay()
	if err != nil {
		f.Close()
		return nil, err
	}
	// Drop a torn tail (if any) and position the file for appends.
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// replay applies every intact record and returns the offset just after the last one.
func (s *Store) replay() (int64, error) {
	r := bufio.NewReader(s.f)
	var offset int64
	header := make([]byte, headerSize)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			return offset, nil // clean end of file, or a header cut short: stop here
		}
		n := binary.BigEndian.Uint32(header[0:4])
		sum := binary.BigEndian.Uint32(header[4:8])
		if n < 5 || n > maxRecord {
			return offset, nil // damaged length field
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return offset, nil // payload cut short
		}
		if crc32.Checksum(payload, castagnoli) != sum {
			return offset, nil // bytes damaged
		}
		if err := s.apply(payload); err != nil {
			return offset, nil
		}
		offset += int64(headerSize) + int64(n)
	}
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
func (s *Store) appendRecord(rec []byte) error {
	if _, err := s.f.Write(rec); err != nil {
		return err
	}
	return s.f.Sync()
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
