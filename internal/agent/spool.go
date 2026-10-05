package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The spool is where every closed window goes before it is pushed, so nothing
// is lost while the backend is unreachable, and nothing is lost across a
// restart either.
//
// Layout: append-only segment files, one JSON Record per line, named after the
// first record's sequence number (00000000000000001234.seg), plus a cursor file
// holding "<segment> <byte offset>" - the first record not yet acknowledged.
// Pushing reads from the cursor forward, oldest first; an acknowledgement moves
// the cursor and deletes segments it has moved past.
//
// Two caps bound it: segments older than maxAge and, oldest first, whatever
// exceeds maxBytes are dropped. Losing the oldest data is the right trade when
// the backend has been gone for two days.
const (
	segmentExt      = ".seg"
	cursorFile      = "cursor"
	segmentMaxBytes = 1 << 20
	// Largest single record read back; a window with 200 containers is ~70 KB.
	maxRecordBytes = 8 << 20
)

// Mark is a position in the spool: just after the last record of a Peek.
type Mark struct {
	Segment string
	Offset  int64
}

type Spool struct {
	dir      string
	maxAge   time.Duration
	maxBytes int64
	now      func() time.Time

	mu         sync.Mutex
	cursor     Mark
	active     *os.File
	activeName string
	activeSize int64
}

// OpenSpool opens (creating if needed) the spool in dir.
func OpenSpool(dir string, maxAge time.Duration, maxBytes int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, maxAge: maxAge, maxBytes: maxBytes, now: time.Now}
	s.cursor = s.readCursor()
	segs, err := s.segments()
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		s.cursor = Mark{}
	} else if s.cursor.Segment == "" || !contains(segs, s.cursor.Segment) {
		// No cursor, or it points at a segment that is gone: everything
		// still on disk is pending.
		s.cursor = Mark{Segment: segs[0]}
	}
	// A crash may have left a half-written line at the end of the last
	// segment. Appends always start a fresh segment after a restart, and the
	// reader skips a line it cannot parse, so the torn tail is harmless.
	return s, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Close releases the active segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil
	}
	err := s.active.Close()
	s.active = nil
	return err
}

// Append writes one record durably (fsync), then enforces the caps.
func (s *Spool) Append(rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.activeSize+int64(len(line)) > segmentMaxBytes {
		if err := s.rotate(rec.Seq); err != nil {
			return err
		}
	}
	if _, err := s.active.Write(line); err != nil {
		return err
	}
	s.activeSize += int64(len(line))
	if err := s.active.Sync(); err != nil {
		return err
	}
	if s.cursor.Segment == "" {
		s.cursor = Mark{Segment: s.activeName}
	}
	return s.enforceCaps()
}

func (s *Spool) rotate(seq uint64) error {
	if s.active != nil {
		s.active.Close()
		s.active = nil
	}
	// Segments are read in name order, so a new one must sort after every
	// one on disk even when the sequence starts over (a state.json that was
	// lost or unreadable): named after seq, it would sort before the pending
	// ones, behind the cursor, and never be pushed.
	if segs, err := s.segments(); err == nil && len(segs) > 0 {
		last, err := strconv.ParseUint(strings.TrimSuffix(segs[len(segs)-1], segmentExt), 10, 64)
		if err == nil && last >= seq {
			seq = last + 1
		}
	}
	name := fmt.Sprintf("%020d%s", seq, segmentExt)
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.active, s.activeName, s.activeSize = f, name, info.Size()
	return nil
}

// Peek returns up to max pending records, oldest first, and the mark to Ack
// once they have been delivered.
func (s *Spool) Peek(max int) ([]Record, Mark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	mark := s.cursor
	if mark.Segment == "" {
		return nil, mark, nil
	}
	segs, err := s.segments()
	if err != nil {
		return nil, mark, err
	}
	for _, seg := range segs {
		if seg < mark.Segment {
			continue
		}
		offset := int64(0)
		if seg == mark.Segment {
			offset = mark.Offset
		}
		recs, end, err := readSegment(filepath.Join(s.dir, seg), offset, max-len(out))
		if err != nil {
			return nil, s.cursor, err
		}
		out = append(out, recs...)
		mark = Mark{Segment: seg, Offset: end}
		if len(out) >= max {
			break
		}
	}
	return out, mark, nil
}

// readSegment reads up to max records from offset. It returns the offset
// after the last line consumed (records it could not parse are skipped).
func readSegment(path string, offset int64, max int) ([]Record, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var out []Record
	pos := offset
	for len(out) < max {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			// End of file, or a torn tail still being written / left by a
			// crash: stop before it.
			break
		}
		pos += int64(len(line))
		if len(line) > maxRecordBytes {
			continue
		}
		var rec Record
		if json.Unmarshal(bytes.TrimSpace(line), &rec) == nil {
			out = append(out, rec)
		}
		if err != nil {
			break
		}
	}
	return out, pos, nil
}

// Ack marks everything up to m as delivered and removes spent segments.
func (s *Spool) Ack(m Mark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = m
	segs, err := s.segments()
	if err != nil {
		return err
	}
	for _, seg := range segs {
		if seg >= m.Segment {
			break
		}
		s.remove(seg)
	}
	// The cursor's own segment is spent once it is read to the end and no
	// longer being written.
	if m.Segment != s.activeName {
		if info, err := os.Stat(filepath.Join(s.dir, m.Segment)); err == nil && m.Offset >= info.Size() {
			s.remove(m.Segment)
			s.cursor = s.firstPending()
		}
	}
	return s.writeCursor()
}

// Pending reports the records and bytes not yet acknowledged.
func (s *Spool) Pending() (records int, size int64, err error) {
	recs, _, err := s.Peek(1 << 30)
	if err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	segs, _ := s.segments()
	for _, seg := range segs {
		if seg < s.cursor.Segment {
			continue
		}
		if info, err := os.Stat(filepath.Join(s.dir, seg)); err == nil {
			size += info.Size()
			if seg == s.cursor.Segment {
				size -= s.cursor.Offset
			}
		}
	}
	return len(recs), size, nil
}

// enforceCaps drops the oldest segments past the age or size cap. Called with
// the lock held.
func (s *Spool) enforceCaps() error {
	segs, err := s.segments()
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-s.maxAge)
	var total int64
	sizes := make(map[string]int64, len(segs))
	for _, seg := range segs {
		info, err := os.Stat(filepath.Join(s.dir, seg))
		if err != nil {
			continue
		}
		sizes[seg] = info.Size()
		total += info.Size()
		if seg != s.activeName && s.maxAge > 0 && info.ModTime().Before(cutoff) {
			total -= info.Size()
			s.remove(seg)
			delete(sizes, seg)
		}
	}
	for _, seg := range segs {
		if s.maxBytes <= 0 || total <= s.maxBytes || seg == s.activeName {
			break
		}
		if size, ok := sizes[seg]; ok {
			total -= size
			s.remove(seg)
		}
	}
	if s.cursor.Segment != "" && !s.exists(s.cursor.Segment) {
		s.cursor = s.firstPending()
		return s.writeCursor()
	}
	return nil
}

func (s *Spool) firstPending() Mark {
	segs, _ := s.segments()
	if len(segs) == 0 {
		return Mark{}
	}
	return Mark{Segment: segs[0]}
}

func (s *Spool) exists(seg string) bool {
	_, err := os.Stat(filepath.Join(s.dir, seg))
	return err == nil
}

func (s *Spool) remove(seg string) {
	if seg == s.activeName && s.active != nil {
		s.active.Close()
		s.active, s.activeName, s.activeSize = nil, "", 0
	}
	os.Remove(filepath.Join(s.dir, seg))
}

func (s *Spool) segments() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), segmentExt) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // zero-padded sequence numbers sort in order
	return out, nil
}

func (s *Spool) readCursor() Mark {
	raw, err := os.ReadFile(filepath.Join(s.dir, cursorFile))
	if err != nil {
		return Mark{}
	}
	seg, off, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	if !ok {
		return Mark{}
	}
	n, err := strconv.ParseInt(off, 10, 64)
	if err != nil || n < 0 {
		return Mark{}
	}
	return Mark{Segment: seg, Offset: n}
}

func (s *Spool) writeCursor() error {
	tmp := filepath.Join(s.dir, cursorFile+".tmp")
	data := fmt.Sprintf("%s %d\n", s.cursor.Segment, s.cursor.Offset)
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, cursorFile))
}
