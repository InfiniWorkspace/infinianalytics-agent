package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rec(seq uint64) Record {
	return Record{Seq: seq, Samples: []HostSample{{TS: time.Unix(int64(seq)*10, 0).UTC(), N: 5, DurS: 10}}}
}

func seqs(recs []Record) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSpoolOrderAndAck(t *testing.T) {
	s, err := OpenSpool(t.TempDir(), time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := uint64(1); i <= 5; i++ {
		if err := s.Append(rec(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, mark, err := s.Peek(3)
	if err != nil || !equal(seqs(got), []uint64{1, 2, 3}) {
		t.Fatalf("peek = %v, %v", seqs(got), err)
	}
	// Peeking again without an ack returns the same records.
	again, _, _ := s.Peek(3)
	if !equal(seqs(again), []uint64{1, 2, 3}) {
		t.Fatalf("second peek = %v", seqs(again))
	}
	if err := s.Ack(mark); err != nil {
		t.Fatal(err)
	}
	rest, mark, _ := s.Peek(10)
	if !equal(seqs(rest), []uint64{4, 5}) {
		t.Fatalf("after ack = %v", seqs(rest))
	}
	s.Ack(mark)
	if n, _, _ := s.Pending(); n != 0 {
		t.Fatalf("pending after draining = %d", n)
	}
}

func TestSpoolSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenSpool(dir, time.Hour, 1<<30)
	for i := uint64(1); i <= 4; i++ {
		s.Append(rec(i))
	}
	_, mark, _ := s.Peek(2)
	s.Ack(mark)
	s.Close()

	// A crash can leave half a line at the end; the reader must stop before it.
	segs, _ := filepath.Glob(filepath.Join(dir, "*"+segmentExt))
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":99,"samp`)
	f.Close()

	s2, err := OpenSpool(dir, time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _, _ := s2.Peek(10)
	if !equal(seqs(got), []uint64{3, 4}) {
		t.Fatalf("after restart = %v", seqs(got))
	}
	// New records go to a new segment, after the old ones.
	s2.Append(rec(5))
	got, _, _ = s2.Peek(10)
	if !equal(seqs(got), []uint64{3, 4, 5}) {
		t.Fatalf("after restart + append = %v", seqs(got))
	}
}

func TestSpoolKeepsOrderWhenTheSequenceStartsOver(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenSpool(dir, time.Hour, 1<<30)
	for i := uint64(500); i <= 502; i++ {
		s.Append(rec(i))
	}
	_, mark, _ := s.Peek(1)
	s.Ack(mark)
	s.Close()

	// A restart that lost state.json numbers records from 1 again.
	s2, _ := OpenSpool(dir, time.Hour, 1<<30)
	defer s2.Close()
	s2.Append(rec(1))
	s2.Append(rec(2))
	got, _, _ := s2.Peek(10)
	if !equal(seqs(got), []uint64{501, 502, 1, 2}) {
		t.Fatalf("pending = %v, want the old records then the new ones", seqs(got))
	}
}

func TestSpoolDropsOldestPastTheByteCap(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenSpool(dir, 0, 1) // tiny cap: only the active segment survives
	defer s.Close()
	for i := uint64(1); i <= 3; i++ {
		s.Append(rec(i))
		s.Close() // force a new segment per record
	}
	got, _, _ := s.Peek(10)
	if !equal(seqs(got), []uint64{3}) {
		t.Fatalf("after byte cap = %v", seqs(got))
	}
}

func TestSpoolDropsSegmentsPastTheAgeCap(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenSpool(dir, 48*time.Hour, 1<<30)
	defer s.Close()
	s.Append(rec(1))
	s.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*"+segmentExt))
	old := time.Now().Add(-49 * time.Hour)
	os.Chtimes(segs[0], old, old)

	s.Append(rec(2))
	got, _, _ := s.Peek(10)
	if !equal(seqs(got), []uint64{2}) {
		t.Fatalf("after age cap = %v", seqs(got))
	}
}

func TestSpoolDeletesSpentSegments(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenSpool(dir, time.Hour, 1<<30)
	defer s.Close()
	s.Append(rec(1))
	s.Close() // rotate
	s.Append(rec(2))
	_, mark, _ := s.Peek(10)
	s.Ack(mark)
	segs, _ := filepath.Glob(filepath.Join(dir, "*"+segmentExt))
	if len(segs) != 1 {
		t.Fatalf("segments left = %v (only the active one should remain)", segs)
	}
}
