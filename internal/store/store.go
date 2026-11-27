// Package store keeps a replica's keys in memory and makes every change durable in an
// append-only log on disk, replayed at start (ADR-0013). From M3 the same log is the
// replication log (ADR-0022): followers receive the leader's records byte for byte.
//
// Record format (integers big-endian); new records are written in format 2 (ADR-0028),
// format 1 records (M3) are still read, with stamp 0:
//
//	[1 B format version][4 B body length n]
//	body, format 1: [8 B epoch][8 B LSN][8 B prev hash][1 B op: 1=put, 2=delete][2 B key length][key][value]
//	body, format 2: [8 B epoch][8 B LSN][8 B prev hash][8 B HLC stamp][1 B op][2 B key length][key][value]
//	[4 B CRC-32C over everything before it]
//
// The stamp is a hybrid logical clock: physical milliseconds in the high 48 bits, a
// counter in the low 16. A leader's own stamps rise with every record; a key's stamps
// rise with every write to it (last-write-wins, ADR-0028).
//
// LSNs start at 1 and grow by 1. "prev hash" is xxhash64 of the previous record's bytes
// (0 for the first), so two logs that differ anywhere differ in every later link. A key's
// version is (epoch, LSN) of its last write; a delete leaves a tombstone with its version.
//
// A damaged record is handled by where it is (ADR-0018):
//
//   - at the end of the file (cut short, or its declared end reaches the end of the
//     file): a crash during an append. Replay keeps everything before it and cuts it off;
//   - followed by more bytes: damage in the middle, which a crash can't cause. Open fails
//     and names the offset; a person decides (or rebuilds the replica from the leader).
//
// A failed write is cut back the same way before the error is returned.
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
	"sort"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
)

const (
	OpPut    byte = 1
	OpDelete byte = 2

	format1       byte = 1                 // M3: no stamp
	formatVersion byte = 2                 // M4: with the HLC stamp; every new record
	prefixSize         = 1 + 4             // format version + body length
	bodyFixed1         = 8 + 8 + 8 + 1 + 2 // epoch, LSN, prev hash, op, key length
	bodyFixed          = bodyFixed1 + 8    // format 2 adds the stamp
	sumSize            = 4                 // CRC-32C
	Overhead           = prefixSize + bodyFixed + sumSize
	maxRecord          = 8 << 20 // 8 MiB: anything longer is treated as a damaged length field
	maxKey             = 1<<16 - 1
)

// castagnoli is CRC-32C, the checksum variant with hardware support on x86 and ARM.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrDamaged: the log is damaged somewhere other than its last record (ADR-0018).
	ErrDamaged = errors.New("log damaged in the middle")
	// ErrGap: a replicated record doesn't follow the last one (a missing LSN).
	ErrGap = errors.New("record doesn't follow the last one")
	// ErrBrokenChain: a record's prev hash doesn't match this log's last record, so this
	// log and the sender's disagree about an earlier record (ADR-0022).
	ErrBrokenChain = errors.New("hash chain broken")
	// ErrStaleEpoch: a write carries an epoch older than the log's last record.
	ErrStaleEpoch = errors.New("epoch older than the log's")
	// ErrSuperseded: the key's latest record has a stamp at least as high as the write's,
	// so the write loses (last-write-wins, ADR-0028) and isn't appended.
	ErrSuperseded = errors.New("superseded by a newer write")
	// ErrPrecondition: an If-Match or If-None-Match condition doesn't hold.
	ErrPrecondition = errors.New("precondition failed")
)

// Version orders writes: epoch first, then LSN.
type Version struct {
	Epoch uint64 `json:"epoch"`
	LSN   uint64 `json:"lsn"`
}

// Less reports whether v is older than o.
func (v Version) Less(o Version) bool {
	if v.Epoch != o.Epoch {
		return v.Epoch < o.Epoch
	}
	return v.LSN < o.LSN
}

// Record is one decoded log record.
type Record struct {
	Epoch, LSN, Prev uint64
	Stamp            uint64 // HLC stamp; 0 for format 1 records
	Op               byte
	Key              string
	Value            []byte
	format           byte // as read; 0 means "write in the current format"
}

// Version returns the record's version.
func (r Record) Version() Version { return Version{r.Epoch, r.LSN} }

// Entry is what the store holds for a key: its last value or a tombstone, and its version
// and stamp.
type Entry struct {
	Value   []byte
	Version Version
	Stamp   uint64
	Deleted bool
}

// Head is a key's latest record in the log, committed or not: what last-write-wins and
// conditional writes compare against (ADR-0028).
type Head struct {
	Stamp   uint64
	LSN     uint64
	Deleted bool
}

// meta is what the store remembers about each record without reading the file; the
// record with LSN n is meta[n-1].
type meta struct {
	offset int64
	epoch  uint64
	hash   uint64
}

// logFile is what the store needs from the log file for writing; *os.File has it. Tests
// swap in a file that fails on purpose.
type logFile interface {
	io.Writer
	io.Seeker
	io.Closer
	Sync() error
	Truncate(size int64) error
}

// Store is a durable key-value map over a log. Safe for concurrent use.
type Store struct {
	mu    sync.RWMutex
	data  map[string]Entry
	heads map[string]Head // each key's latest record in the log
	clock uint64          // the highest stamp in the log: the HLC's last value
	recs  []meta
	path  string
	f     logFile  // appends
	rf    *os.File // reads (ReadAfter, rebuild after truncation); ReadAt is safe concurrently
	end   int64    // offset just after the last good record: where the next append goes
	// broken is set when a failed write couldn't be cut back: from then on every write
	// fails, so nothing is ever appended after garbage. Cleared only by a restart.
	broken error
}

// Open loads (or creates) the log in dir and replays it into memory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "data.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	rf, err := os.Open(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	s := &Store{path: path, f: f, rf: rf}
	info, err := f.Stat()
	if err != nil {
		s.closeFiles()
		return nil, err
	}
	good, err := s.replay(io.NewSectionReader(rf, 0, info.Size()), info.Size())
	if err != nil {
		s.closeFiles() // nothing is cut: the file stays as it was for a person to look at
		return nil, err
	}
	s.end = good
	// Drop a torn tail (if any) and position the file for appends.
	if err := s.cutBack(); err != nil {
		s.closeFiles()
		return nil, err
	}
	return s, nil
}

func (s *Store) closeFiles() {
	s.f.Close()
	s.rf.Close()
}

// replay rebuilds the map and the record index from the first size bytes of r and returns
// the offset just after the last good record. A damaged record that isn't the last thing
// in the file is an error (ErrDamaged).
func (s *Store) replay(r io.Reader, size int64) (int64, error) {
	s.data = map[string]Entry{}
	s.heads = map[string]Head{}
	s.clock = 0
	s.recs = s.recs[:0]
	br := bufio.NewReader(r)
	var offset int64
	prefix := make([]byte, prefixSize)

	// damaged decides what a bad record at offset means, from where it says it ends.
	damaged := func(end int64, what string) (int64, error) {
		if end >= size {
			return offset, nil // the last record: torn by a crash, cut off by Open
		}
		return offset, fmt.Errorf("%w: %s: %s at offset %d, %d more bytes follow; "+
			"restore the file, or rebuild the replica from the leader",
			ErrDamaged, s.path, what, offset, size-end)
	}

	for {
		if _, err := io.ReadFull(br, prefix); err != nil {
			return offset, nil // clean end of file, or a prefix cut short: a torn tail
		}
		n := int64(binary.BigEndian.Uint32(prefix[1:5]))
		end := offset + prefixSize + n + sumSize
		if prefix[0] != formatVersion && prefix[0] != format1 {
			return damaged(end, fmt.Sprintf("unknown format version %d", prefix[0]))
		}
		if n < int64(bodyFixedOf(prefix[0])) || n > maxRecord {
			return damaged(end, "impossible record length")
		}
		b := make([]byte, prefixSize+n+sumSize)
		copy(b, prefix)
		if _, err := io.ReadFull(br, b[prefixSize:]); err != nil {
			return offset, nil // runs past the end of the file: a torn tail
		}
		rec, err := decode(b)
		if err != nil {
			return damaged(end, err.Error())
		}
		if err := s.follows(rec); err != nil {
			return damaged(end, err.Error())
		}
		s.recs = append(s.recs, meta{offset: offset, epoch: rec.Epoch, hash: xxhash.Sum64(b)})
		s.noteHead(rec)
		s.apply(rec)
		offset = end
	}
}

// follows checks that rec can come right after the log's last record. Callers hold the lock.
func (s *Store) follows(rec Record) error {
	last, hash := s.lastLocked()
	switch {
	case rec.LSN != last.LSN+1:
		return fmt.Errorf("%w: LSN %d after %d", ErrGap, rec.LSN, last.LSN)
	case rec.Prev != hash:
		return fmt.Errorf("%w at LSN %d", ErrBrokenChain, rec.LSN)
	case rec.Epoch < last.Epoch:
		return fmt.Errorf("%w: epoch %d after %d", ErrBrokenChain, rec.Epoch, last.Epoch)
	}
	return nil
}

// bodyFixedOf is the fixed part of a body in the given format.
func bodyFixedOf(format byte) int {
	if format == format1 {
		return bodyFixed1
	}
	return bodyFixed
}

// encode builds the bytes of one complete record, in the format it was read in (a
// follower must store the leader's bytes exactly, or the hash chain breaks), or in the
// current format for a new record.
func (r Record) encode() []byte {
	format := r.format
	if format == 0 {
		format = formatVersion
	}
	fixed := bodyFixedOf(format)
	n := fixed + len(r.Key) + len(r.Value)
	b := make([]byte, prefixSize+n+sumSize)
	b[0] = format
	binary.BigEndian.PutUint32(b[1:5], uint32(n))
	p := b[prefixSize:]
	binary.BigEndian.PutUint64(p[0:8], r.Epoch)
	binary.BigEndian.PutUint64(p[8:16], r.LSN)
	binary.BigEndian.PutUint64(p[16:24], r.Prev)
	at := 24
	if format != format1 {
		binary.BigEndian.PutUint64(p[24:32], r.Stamp)
		at = 32
	}
	p[at] = r.Op
	binary.BigEndian.PutUint16(p[at+1:at+3], uint16(len(r.Key)))
	copy(p[fixed:], r.Key)
	copy(p[fixed+len(r.Key):], r.Value)
	binary.BigEndian.PutUint32(b[prefixSize+n:], crc32.Checksum(b[:prefixSize+n], castagnoli))
	return b
}

// decode parses the bytes of one complete record and checks its checksum.
func decode(b []byte) (Record, error) {
	if len(b) < prefixSize+bodyFixed1+sumSize || (b[0] != formatVersion && b[0] != format1) {
		return Record{}, errors.New("not a record")
	}
	fixed := bodyFixedOf(b[0])
	n := int(binary.BigEndian.Uint32(b[1:5]))
	if prefixSize+n+sumSize != len(b) {
		return Record{}, errors.New("length field doesn't match the record")
	}
	if crc32.Checksum(b[:prefixSize+n], castagnoli) != binary.BigEndian.Uint32(b[prefixSize+n:]) {
		return Record{}, errors.New("checksum mismatch")
	}
	if n < fixed {
		return Record{}, errors.New("record shorter than its fixed fields")
	}
	p := b[prefixSize : prefixSize+n]
	rec := Record{
		Epoch:  binary.BigEndian.Uint64(p[0:8]),
		LSN:    binary.BigEndian.Uint64(p[8:16]),
		Prev:   binary.BigEndian.Uint64(p[16:24]),
		format: b[0],
	}
	at := 24
	if b[0] != format1 {
		rec.Stamp = binary.BigEndian.Uint64(p[24:32])
		at = 32
	}
	rec.Op = p[at]
	klen := int(binary.BigEndian.Uint16(p[at+1 : at+3]))
	if fixed+klen > len(p) {
		return Record{}, errors.New("key length past end of record")
	}
	if rec.Op != OpPut && rec.Op != OpDelete {
		return Record{}, fmt.Errorf("unknown op %d", rec.Op)
	}
	rec.Key = string(p[fixed : fixed+klen])
	rec.Value = append([]byte(nil), p[fixed+klen:]...)
	return rec, nil
}

// DecodeRecords splits bytes from ReadAfter into records, checking each checksum.
func DecodeRecords(raw []byte) ([]Record, error) {
	var recs []Record
	for len(raw) > 0 {
		if len(raw) < prefixSize {
			return nil, errors.New("records cut short")
		}
		size := prefixSize + int(binary.BigEndian.Uint32(raw[1:5])) + sumSize
		if size > len(raw) {
			return nil, errors.New("records cut short")
		}
		rec, err := decode(raw[:size])
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", len(recs)+1, err)
		}
		recs = append(recs, rec)
		raw = raw[size:]
	}
	return recs, nil
}

// apply puts a record into the map, unless the map already holds a newer version of the
// key: on a leader, commits can finish in any order (ADR-0023). Callers hold the lock.
func (s *Store) apply(rec Record) {
	if cur, ok := s.data[rec.Key]; ok && !cur.Version.Less(rec.Version()) {
		return
	}
	e := Entry{Version: rec.Version(), Stamp: rec.Stamp, Deleted: rec.Op == OpDelete}
	if !e.Deleted {
		e.Value = rec.Value
	}
	s.data[rec.Key] = e
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

// appendBytes writes whole records and waits until the disk has them (fsync). Callers hold
// the write lock. If the write or the fsync fails, part of it may already be in the file:
// it is cut back to the last good record before the error is returned (ADR-0018).
func (s *Store) appendBytes(b []byte) error {
	if s.broken != nil {
		return s.broken
	}
	_, err := s.f.Write(b)
	if err == nil {
		err = s.f.Sync()
	}
	if err == nil {
		s.end += int64(len(b))
		return nil
	}
	if cerr := s.cutBack(); cerr != nil {
		s.broken = fmt.Errorf("store stopped accepting writes: a write failed (%v) and the log could not be cut back (%v)", err, cerr)
	}
	return err
}

// Write appends the next record of the given epoch to the log, stamped by the store's
// clock, and flushes it, but doesn't apply it: a leader applies only after the write
// commits (ADR-0023), with Apply.
func (s *Store) Write(epoch uint64, op byte, key string, value []byte) (Record, error) {
	rec, _, err := s.WriteVersioned(epoch, op, key, value, WriteOpts{}, nowMillis())
	return rec, err
}

// WriteOpts are a leader write's version rules (ADR-0028).
type WriteOpts struct {
	Stamp       uint64 // a stamp the client sends back (a retry); 0 = the store stamps it
	IfMatch     uint64 // with HasIfMatch: apply only if this is the key's latest stamp
	HasIfMatch  bool
	IfNoneMatch bool // apply only if the key has no live latest record (create only)
}

// WriteVersioned is Write with last-write-wins and conditions, checked and appended under
// one lock so no other write to the key can slip in between. It returns the key's head
// before the write: on ErrSuperseded it is the record that wins, which the leader waits
// to see committed before it answers. nowMs is the leader's physical clock.
func (s *Store) WriteVersioned(epoch uint64, op byte, key string, value []byte, o WriteOpts, nowMs uint64) (Record, Head, error) {
	if len(key) > maxKey || bodyFixed+len(key)+len(value) > maxRecord {
		return Record{}, Head{}, errors.New("record too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	head, exists := s.heads[key]
	switch {
	case o.HasIfMatch && (!exists || head.Stamp != o.IfMatch):
		return Record{}, head, ErrPrecondition
	case o.IfNoneMatch && exists && !head.Deleted:
		return Record{}, head, ErrPrecondition
	}
	stamp := o.Stamp
	if stamp == 0 {
		stamp = max(nowMs<<16, s.clock+1)
	} else if exists && stamp <= head.Stamp {
		s.clock = max(s.clock, stamp) // HLC receive rule, even for a write that loses
		return Record{}, head, ErrSuperseded
	}
	rec, err := s.writeLocked(epoch, stamp, op, key, value)
	return rec, head, err
}

func (s *Store) writeLocked(epoch, stamp uint64, op byte, key string, value []byte) (Record, error) {
	last, hash := s.lastLocked()
	if epoch < last.Epoch {
		return Record{}, fmt.Errorf("%w: %d < %d", ErrStaleEpoch, epoch, last.Epoch)
	}
	rec := Record{Epoch: epoch, LSN: last.LSN + 1, Prev: hash, Stamp: stamp, Op: op, Key: key,
		Value: append([]byte(nil), value...)} // own copy: the caller may reuse its buffer
	b := rec.encode()
	offset := s.end
	if err := s.appendBytes(b); err != nil {
		return Record{}, err
	}
	s.recs = append(s.recs, meta{offset: offset, epoch: epoch, hash: xxhash.Sum64(b)})
	s.noteHead(rec)
	return rec, nil
}

// noteHead records rec as its key's latest record and moves the clock past its stamp.
// Callers hold the lock.
func (s *Store) noteHead(rec Record) {
	s.heads[rec.Key] = Head{Stamp: rec.Stamp, LSN: rec.LSN, Deleted: rec.Op == OpDelete}
	s.clock = max(s.clock, rec.Stamp)
}

// Head returns the key's latest record in the log, committed or not.
func (s *Store) Head(key string) (Head, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.heads[key]
	return h, ok
}

// Clock returns the highest stamp in the log.
func (s *Store) Clock() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clock
}

// nowMillis is the physical clock in Unix milliseconds.
func nowMillis() uint64 { return uint64(time.Now().UnixMilli()) }

// Apply makes a written record visible to reads (see Write).
func (s *Store) Apply(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apply(rec)
}

// Replicate appends records received from the leader (bytes from its ReadAfter) and
// applies them. Each record's checksum and hash link are checked first; records the log
// already has are skipped. All of them are flushed with one fsync.
func (s *Store) Replicate(raw []byte) error {
	recs, err := DecodeRecords(raw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, hash := s.lastLocked()
	var buf []byte
	var added []meta
	offset := s.end
	for i, rec := range recs {
		if rec.LSN <= last.LSN {
			continue // already have it
		}
		if rec.LSN != last.LSN+1 {
			return fmt.Errorf("%w: LSN %d after %d", ErrGap, rec.LSN, last.LSN)
		}
		if rec.Prev != hash || rec.Epoch < last.Epoch {
			return fmt.Errorf("%w at LSN %d", ErrBrokenChain, rec.LSN)
		}
		b := recs[i].encode() // identical to the leader's bytes: same fields, same layout
		h := xxhash.Sum64(b)
		added = append(added, meta{offset: offset + int64(len(buf)), epoch: rec.Epoch, hash: h})
		buf = append(buf, b...)
		last, hash = rec.Version(), h
	}
	if len(buf) == 0 {
		return nil
	}
	if err := s.appendBytes(buf); err != nil {
		return err
	}
	s.recs = append(s.recs, added...)
	for _, rec := range recs[len(recs)-len(added):] {
		s.noteHead(rec)
		s.apply(rec)
	}
	return nil
}

// ReadAfter returns the bytes of up to max records with an LSN above lsn, and how many.
func (s *Store) ReadAfter(lsn uint64, max int) ([]byte, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if lsn >= uint64(len(s.recs)) || max <= 0 {
		return nil, 0, nil
	}
	from := s.recs[lsn].offset
	upto := uint64(len(s.recs))
	if lsn+uint64(max) < upto {
		upto = lsn + uint64(max)
	}
	to := s.end
	if upto < uint64(len(s.recs)) {
		to = s.recs[upto].offset
	}
	b := make([]byte, to-from)
	if _, err := s.rf.ReadAt(b, from); err != nil {
		return nil, 0, err
	}
	return b, int(upto - lsn), nil
}

// Size returns the log's length in bytes.
func (s *Store) Size() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.end
}

// Last returns the version and hash of the last record (zeros for an empty log).
func (s *Store) Last() (Version, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastLocked()
}

func (s *Store) lastLocked() (Version, uint64) {
	n := len(s.recs)
	if n == 0 {
		return Version{}, 0
	}
	return Version{s.recs[n-1].epoch, uint64(n)}, s.recs[n-1].hash
}

// HashAt returns the hash of the record with the given LSN (0 for LSN 0) and whether the
// log has it. A follower's last record is in this log only if both match.
func (s *Store) HashAt(lsn uint64) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if lsn == 0 {
		return 0, true
	}
	if lsn > uint64(len(s.recs)) {
		return 0, false
	}
	return s.recs[lsn-1].hash, true
}

// LastLSNOfEpoch returns the LSN of the last record written in an epoch up to and
// including epoch: where a returning replica of that epoch must cut its log (ADR-0023).
func (s *Store) LastLSNOfEpoch(epoch uint64) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Epochs never decrease along the log, so the records of epochs <= epoch are a prefix.
	return uint64(sort.Search(len(s.recs), func(i int) bool { return s.recs[i].epoch > epoch }))
}

// TruncateAfter cuts the log back so that lsn is its last record, and rebuilds the map
// from what remains. Used when a returning replica holds records the leader doesn't.
func (s *Store) TruncateAfter(lsn uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lsn >= uint64(len(s.recs)) {
		return nil
	}
	s.end = s.recs[lsn].offset
	if err := s.cutBack(); err != nil {
		return err
	}
	_, err := s.replay(io.NewSectionReader(s.rf, 0, s.end), s.end)
	return err
}

// Put writes and applies a value at once, in the log's current epoch. For stores that
// aren't replicated (the coordinator's registry). It returns after the change is on disk.
func (s *Store) Put(key string, value []byte) error {
	if len(key) > maxKey || bodyFixed+len(key)+len(value) > maxRecord {
		return errors.New("record too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last, _ := s.lastLocked()
	rec, err := s.writeLocked(last.Epoch, max(nowMillis()<<16, s.clock+1), OpPut, key, value)
	if err != nil {
		return err
	}
	s.apply(rec)
	return nil
}

// Delete removes key and reports whether it existed. A missing key writes nothing.
func (s *Store) Delete(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.data[key]; !ok || e.Deleted {
		return false, nil
	}
	last, _ := s.lastLocked()
	rec, err := s.writeLocked(last.Epoch, max(nowMillis()<<16, s.clock+1), OpDelete, key, nil)
	if err != nil {
		return false, err
	}
	s.apply(rec)
	return true, nil
}

// Get returns the value for key and whether it exists (a tombstone doesn't).
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[key]
	if !ok || e.Deleted {
		return nil, false
	}
	return e.Value, true
}

// Lookup returns the key's entry, tombstones included, with its version.
func (s *Store) Lookup(key string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[key]
	return e, ok
}

// Keys returns every live key (for listing tables; not used on hot paths).
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for k, e := range s.data {
		if !e.Deleted {
			keys = append(keys, k)
		}
	}
	return keys
}

// Len returns the number of live keys.
func (s *Store) Len() int {
	return len(s.Keys())
}

// Close flushes and closes the log.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.f.Close()
	s.rf.Close()
	return err
}
