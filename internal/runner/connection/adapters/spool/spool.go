// Package spool is the runner's bounded on-disk event spool (KIN-22): events are appended (fsync) before they are
// sent and deleted only after the Control Plane acknowledged them, so a disconnect or a runner restart loses
// nothing that was reported.
//
// Layout in the spool directory (the data dir's events/, 0700): segment files seg-<first seq>.log (0600) of framed
// records — 4-byte big-endian length, 4-byte CRC-32C of the payload, payload (JSON) — and meta.json with the epoch
// and the acknowledged seq, replaced atomically (temp file, fsync, rename, directory fsync). A torn or corrupt
// record ends the log: it and everything after it are cut off on open.
package spool

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

var _ app.Spool = (*Spool)(nil)

// Defaults: 256 MiB of events (KIN-22 decision), 1 MiB reserved for lifecycle events, 4 MiB segments.
const (
	DefaultLimit   = 256 << 20
	DefaultReserve = 1 << 20
	DefaultSegment = 4 << 20
	headerBytes    = 8
	maxRecordBytes = protocol.MaxMessageBytes
	// ackPersistEvery bounds how many acknowledged events a crash can make the runner send again (the Control
	// Plane drops them as duplicates); deleting a segment always persists the position first.
	ackPersistEvery = 64
)

// Config of a spool. Zero values take the defaults.
type Config struct {
	Dir          string
	LimitBytes   int64 // output is refused above this
	ReserveBytes int64 // extra room for non-output events
	SegmentBytes int64 // a segment is closed after this many bytes
}

type segment struct {
	path        string
	first, last int64 // seqs; last < first while empty
	size        int64
}

type meta struct {
	Epoch string `json:"epoch"`
	Acked int64  `json:"acked"`
}

type attemptKey struct {
	step  string
	fence int64
}

// Spool is safe for concurrent use.
type Spool struct {
	mu        sync.Mutex
	cfg       Config
	meta      meta
	persisted int64 // meta.Acked as last written
	segs      []segment
	active    *os.File
	last      int64
	bytes     int64
	pending   map[attemptKey]app.PendingResult
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt reports an unreadable meta file; the spool refuses to start rather than resetting its epoch silently.
var ErrCorrupt = errors.New("event spool metadata is corrupt")

// Open opens (or creates) the spool in cfg.Dir, recovering from a torn tail.
func Open(cfg Config) (*Spool, error) {
	if cfg.LimitBytes <= 0 {
		cfg.LimitBytes = DefaultLimit
	}
	if cfg.ReserveBytes <= 0 {
		cfg.ReserveBytes = DefaultReserve
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = DefaultSegment
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating event spool: %w", err)
	}
	s := &Spool{cfg: cfg, pending: map[attemptKey]app.PendingResult{}}
	if err := s.loadMeta(); err != nil {
		return nil, err
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Spool) metaPath() string { return filepath.Join(s.cfg.Dir, "meta.json") }

func (s *Spool) loadMeta() error {
	b, err := os.ReadFile(s.metaPath())
	if errors.Is(err, os.ErrNotExist) {
		s.meta = meta{Epoch: rand.Text()}
		return s.writeMeta()
	}
	if err != nil {
		return fmt.Errorf("reading event spool metadata: %w", err)
	}
	if json.Unmarshal(b, &s.meta) != nil || s.meta.Epoch == "" || s.meta.Acked < 0 {
		return ErrCorrupt
	}
	s.persisted = s.meta.Acked
	return nil
}

func (s *Spool) writeMeta() error {
	b, _ := json.Marshal(s.meta)
	if err := writeAtomic(s.metaPath(), b); err != nil {
		return fmt.Errorf("event spool metadata: %w", err)
	}
	s.persisted = s.meta.Acked
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

func segName(first int64) string { return fmt.Sprintf("seg-%020d.log", first) }

// recover scans the segments in order, cuts the log at the first torn or corrupt record and rebuilds the state.
func (s *Spool) recover() error {
	names, err := filepath.Glob(filepath.Join(s.cfg.Dir, "seg-*.log"))
	if err != nil {
		return fmt.Errorf("listing event spool: %w", err)
	}
	type named struct {
		path  string
		first int64
	}
	var found []named
	for _, p := range names {
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "seg-"), ".log"), 10, 64)
		if err != nil || n < 1 {
			continue // not ours
		}
		found = append(found, named{p, n})
	}
	slices.SortFunc(found, func(a, b named) int { return int(a.first - b.first) })
	s.last = s.meta.Acked
	cut := false
	for _, f := range found {
		if cut {
			if err := os.Remove(f.path); err != nil {
				return fmt.Errorf("removing event spool segment after corruption: %w", err)
			}
			continue
		}
		seg := segment{path: f.path, first: f.first, last: f.first - 1}
		good, ok, err := s.scan(&seg)
		if err != nil {
			return err
		}
		if !ok {
			cut = true
			if err := os.Truncate(f.path, good); err != nil {
				return fmt.Errorf("truncating event spool: %w", err)
			}
		}
		seg.size = good
		if seg.last >= seg.first && seg.last <= s.meta.Acked {
			if err := os.Remove(f.path); err != nil {
				return fmt.Errorf("removing acknowledged event spool segment: %w", err)
			}
			continue
		}
		if seg.last > s.last {
			s.last = seg.last
		}
		s.bytes += seg.size
		s.segs = append(s.segs, seg)
	}
	for k, p := range s.pending {
		if p.Seq <= s.meta.Acked {
			delete(s.pending, k)
		}
	}
	if cut {
		// The cut-off seqs may already be applied by the Control Plane (an ACK not yet persisted): a new epoch keeps
		// the seqs written from now on from being taken for duplicates (the event's Base carries the position over).
		s.meta.Epoch = rand.Text()
		return s.writeMeta()
	}
	return nil
}

// scan reads seg's records, tracking pending results; it returns the length of the intact prefix and whether the
// whole file was intact.
func (s *Spool) scan(seg *segment) (int64, bool, error) {
	f, err := os.Open(seg.path)
	if err != nil {
		return 0, false, fmt.Errorf("opening event spool segment: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	var off int64
	for {
		e, n, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			return off, true, nil
		}
		if err != nil || e.Seq != seg.last+1 {
			return off, false, nil
		}
		seg.last = e.Seq
		off += n
		s.track(e)
	}
}

func (s *Spool) track(e app.SpooledEvent) {
	if protocol.TerminalEvent(e.Kind) {
		s.pending[attemptKey{e.Step, e.Fence}] = app.PendingResult{Step: e.Step, Fence: e.Fence, Kind: e.Kind, Seq: e.Seq}
	}
}

var errTorn = errors.New("torn record")

// readRecord reads one framed record; io.EOF only at a clean record boundary.
func readRecord(r *bufio.Reader) (app.SpooledEvent, int64, error) {
	var h [headerBytes]byte
	n, err := io.ReadFull(r, h[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return app.SpooledEvent{}, 0, io.EOF
	}
	if err != nil {
		return app.SpooledEvent{}, 0, errTorn
	}
	size := binary.BigEndian.Uint32(h[:4])
	if size == 0 || size > maxRecordBytes {
		return app.SpooledEvent{}, 0, errTorn
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return app.SpooledEvent{}, 0, errTorn
	}
	if crc32.Checksum(buf, crcTable) != binary.BigEndian.Uint32(h[4:]) {
		return app.SpooledEvent{}, 0, errTorn
	}
	var e app.SpooledEvent
	if err := json.Unmarshal(buf, &e); err != nil {
		return app.SpooledEvent{}, 0, errTorn
	}
	return e, int64(headerBytes) + int64(size), nil
}

// Append writes e durably and returns its seq. Output is refused above LimitBytes, anything else above
// LimitBytes+ReserveBytes (app.ErrSpoolFull).
func (s *Spool) Append(e app.SpooledEvent) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = s.last + 1
	payload, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("encoding spooled event: %w", err)
	}
	if len(payload) > maxRecordBytes {
		return 0, fmt.Errorf("spooled event of %d bytes exceeds %d", len(payload), maxRecordBytes)
	}
	size := int64(headerBytes + len(payload))
	limit := s.cfg.LimitBytes
	if e.Kind != protocol.EventOutput {
		limit += s.cfg.ReserveBytes
	}
	if s.bytes+size > limit {
		return 0, app.ErrSpoolFull
	}
	if err := s.writable(); err != nil {
		return 0, err
	}
	frame := make([]byte, size)
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload))) //nolint:gosec // G115: bounded by maxRecordBytes above
	binary.BigEndian.PutUint32(frame[4:8], crc32.Checksum(payload, crcTable))
	copy(frame[headerBytes:], payload)
	seg := &s.segs[len(s.segs)-1]
	if _, err := s.active.Write(frame); err != nil {
		// Cut the partial write off so the next record starts at a frame boundary.
		_ = s.active.Truncate(seg.size)
		return 0, fmt.Errorf("writing event spool: %w", err)
	}
	if err := s.active.Sync(); err != nil {
		_ = s.active.Truncate(seg.size)
		return 0, fmt.Errorf("syncing event spool: %w", err)
	}
	seg.size += size
	seg.last = e.Seq
	s.bytes += size
	s.last = e.Seq
	s.track(e)
	return e.Seq, nil
}

// writable makes sure the last segment is open for appending and has room.
func (s *Spool) writable() error {
	if s.active != nil && s.segs[len(s.segs)-1].size < s.cfg.SegmentBytes {
		return nil
	}
	if s.active != nil {
		if err := s.active.Close(); err != nil {
			return fmt.Errorf("closing event spool segment: %w", err)
		}
		s.active = nil
	}
	if n := len(s.segs); n > 0 && s.segs[n-1].size < s.cfg.SegmentBytes {
		f, err := os.OpenFile(s.segs[n-1].path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("opening event spool segment: %w", err)
		}
		s.active = f
		return nil
	}
	first := s.last + 1
	path := filepath.Join(s.cfg.Dir, segName(first))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating event spool segment: %w", err)
	}
	if err := syncDir(s.cfg.Dir); err != nil {
		_ = f.Close()
		return err
	}
	s.active = f
	s.segs = append(s.segs, segment{path: path, first: first, last: first - 1})
	return nil
}

// Read returns up to n events after seq, about maxBytes of payload but at least one.
func (s *Spool) Read(after int64, n, maxBytes int) ([]app.SpooledEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.SpooledEvent
	budget := maxBytes
	for _, seg := range s.segs {
		if seg.last <= after || seg.last < seg.first {
			continue
		}
		f, err := os.Open(seg.path)
		if err != nil {
			return nil, fmt.Errorf("opening event spool segment: %w", err)
		}
		r := bufio.NewReader(io.LimitReader(f, seg.size))
		for {
			e, size, err := readRecord(r)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("reading event spool %s: %w", filepath.Base(seg.path), err)
			}
			if e.Seq <= after {
				continue
			}
			if len(out) > 0 && (len(out) >= n || int(size) > budget) {
				_ = f.Close()
				return out, nil
			}
			out = append(out, e)
			budget -= int(size)
		}
		_ = f.Close()
	}
	return out, nil
}

// Ack records that the Control Plane has everything up to seq and deletes the segments it covers.
func (s *Spool) Ack(seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.last {
		seq = s.last
	}
	if seq <= s.meta.Acked {
		return nil
	}
	s.meta.Acked = seq
	for k, p := range s.pending {
		if p.Seq <= seq {
			delete(s.pending, k)
		}
	}
	drop := 0
	for drop < len(s.segs) && s.segs[drop].last <= seq && s.segs[drop].last >= s.segs[drop].first {
		drop++
	}
	if drop == 0 && seq-s.persisted < ackPersistEvery {
		return nil
	}
	if err := s.writeMeta(); err != nil {
		return err
	}
	for _, seg := range s.segs[:drop] {
		if s.active != nil && len(s.segs) > 0 && seg.path == s.segs[len(s.segs)-1].path {
			_ = s.active.Close()
			s.active = nil
		}
		if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing event spool segment: %w", err)
		}
		s.bytes -= seg.size
	}
	s.segs = slices.Delete(s.segs, 0, drop)
	return nil
}

// State returns the spool's position.
func (s *Spool) State() app.SpoolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return app.SpoolState{Epoch: s.meta.Epoch, Acked: s.meta.Acked, Last: s.last, Bytes: s.bytes, LimitBytes: s.cfg.LimitBytes}
}

// Pending lists unacknowledged terminal events, ordered by seq.
func (s *Spool) Pending() []app.PendingResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]app.PendingResult, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b app.PendingResult) int { return int(a.Seq - b.Seq) })
	return out
}

// Close persists the acknowledged position and closes the active segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.meta.Acked != s.persisted {
		err = s.writeMeta()
	}
	if s.active != nil {
		err = errors.Join(err, s.active.Close())
		s.active = nil
	}
	return err
}
