package meter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkTrace(i int) Trace {
	return Trace{
		RequestID:     fmt.Sprintf("req-%06d", i),
		TraceID:       fmt.Sprintf("trace-%06d", i),
		SpanID:        "span-1",
		ParentSpanID:  "span-0",
		Time:          time.Unix(1700000000, int64(i)*1000).UTC(),
		APIKeyID:      "key-1",
		TeamID:        "team-1",
		ModelGroup:    "gpt-4o",
		Provider:      "openai",
		CredentialID:  "cred-1",
		Endpoint:      "/v1/chat/completions",
		UpstreamModel: "gpt-4o-2024-08-06",
		Status:        200,
		Excerpt:       fmt.Sprintf("excerpt %d", i),
		Latency:       time.Duration(i) * time.Millisecond,
		TTFT:          time.Duration(i) * time.Microsecond,
		QueueWait:     3 * time.Microsecond,
		Tokens:        Tokens{Input: int64(i), Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 5},
		CostNano:      int64(i) * 1000,
		Retries:       i % 3,
	}
}

// ------------------------------------------------------------------- codec

func TestTraceCodecRoundTrip(t *testing.T) {
	cases := []Trace{
		mkTrace(1),
		{}, // every field zero
		{RequestID: "req", Excerpt: "유니코드 excerpt with emoji 🎉", CostNano: -5, Status: -1},
		{RequestID: strings.Repeat("x", 4000), Excerpt: strings.Repeat("y", 4000)},
	}
	for i, want := range cases {
		b := appendTrace(nil, &want)
		got, err := decodeTrace(b)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		// Time round-trips at microsecond precision in UTC.
		want.Time = want.Time.UTC().Truncate(time.Microsecond)
		got.Time = got.Time.Truncate(time.Microsecond)
		if got != want {
			t.Errorf("case %d:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

func TestTraceCodecRejectsGarbage(t *testing.T) {
	good := appendTrace(nil, &Trace{RequestID: "req-1", Excerpt: "hello"})
	for _, bad := range [][]byte{
		nil,
		{},
		{99},               // wrong version
		good[:len(good)/2], // truncated
		// Written against the version this build does NOT speak. Spelled
		// relative to the constant rather than as a literal, so that bumping
		// the codec cannot turn this case into "the current version decodes",
		// which is what a hard-coded neighbour silently became.
		append([]byte{traceCodecVer + 1}, good[1:]...),
	} {
		if _, err := decodeTrace(bad); err == nil {
			t.Errorf("decodeTrace(%v...) succeeded on garbage", bad[:min(4, len(bad))])
		}
	}
}

// -------------------------------------------------------------- disk spool

func TestDiskSpoolAppendReadCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := openDiskSpool(dir, 1<<20, 4<<10, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const n = 300
	batch := make([]Trace, n)
	for i := range batch {
		batch[i] = mkTrace(i)
	}
	written, err := s.Append(batch)
	if err != nil || written != n {
		t.Fatalf("Append = %d, %v; want %d, nil", written, err, n)
	}
	if recs, bytes := s.Depth(); recs != n || bytes == 0 {
		t.Fatalf("Depth = %d records, %d bytes; want %d records and non-zero bytes", recs, bytes, n)
	}
	if got := len(s.segs); got < 2 {
		t.Errorf("segments = %d, want at least 2 with a 4 KiB roll size", got)
	}

	var all []Trace
	for {
		before := len(all)
		all, err = s.Next(64, all)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(all) == before {
			break
		}
		if err := s.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	if len(all) != n {
		t.Fatalf("read %d records, want %d", len(all), n)
	}
	for i := range all {
		if all[i].RequestID != batch[i].RequestID {
			t.Fatalf("record %d = %s, want %s -- order not preserved", i, all[i].RequestID, batch[i].RequestID)
		}
	}
	if recs, _ := s.Depth(); recs != 0 {
		t.Errorf("Depth = %d after full commit, want 0", recs)
	}
	// Consumed segments are unlinked, not merely ignored.
	if got := len(s.segs); got > 1 {
		t.Errorf("%d segments remain after full acknowledgement, want at most 1", got)
	}
}

func TestDiskSpoolDoesNotDropUncommittedRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	batch := []Trace{mkTrace(1), mkTrace(2), mkTrace(3)}
	if _, err := s.Append(batch); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Read but do not acknowledge -- as when the sink write fails.
	got, err := s.Next(10, nil)
	if err != nil || len(got) != 3 {
		t.Fatalf("Next = %d, %v", len(got), err)
	}
	if recs, _ := s.Depth(); recs != 3 {
		t.Errorf("Depth = %d before Commit, want 3 -- Next must not consume", recs)
	}
	// Reading again returns the same records.
	again, err := s.Next(10, nil)
	if err != nil || len(again) != 3 || again[0].RequestID != got[0].RequestID {
		t.Fatalf("re-read = %d records, %v; want the same 3", len(again), err)
	}
	s.Close()

	s2, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if recs, _ := s2.Depth(); recs != 3 {
		t.Errorf("Depth after restart = %d, want 3 -- unacknowledged records must replay", recs)
	}
}

func TestDiskSpoolEnforcesSizeCap(t *testing.T) {
	dir := t.TempDir()
	s, err := openDiskSpool(dir, 4<<10, 1<<10, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	batch := make([]Trace, 500)
	for i := range batch {
		batch[i] = mkTrace(i)
	}
	written, err := s.Append(batch)
	if !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("Append error = %v, want ErrSpoolFull", err)
	}
	if written == 0 {
		t.Error("Append accepted nothing at all; the cap should admit what fits")
	}
	if written == len(batch) {
		t.Error("Append accepted everything; the cap is not enforced")
	}
	_, bytes := s.Depth()
	if bytes > 4<<10 {
		t.Errorf("spool holds %d bytes against a %d-byte cap", bytes, 4<<10)
	}

	// Draining makes room again.
	got, err := s.Next(1000, nil)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(got) != written {
		t.Fatalf("read %d, want the %d that were accepted", len(got), written)
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n, err := s.Append(batch[:1]); n != 1 || err != nil {
		t.Errorf("Append after draining = %d, %v; want 1, nil", n, err)
	}
}

func TestDiskSpoolTruncatesTornTail(t *testing.T) {
	dir := t.TempDir()
	s, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	batch := []Trace{mkTrace(1), mkTrace(2), mkTrace(3)}
	if _, err := s.Append(batch); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := s.segs[0].path
	s.Close()

	// Simulate a crash part-way through a fourth record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	partial := appendTrace(nil, &batch[0])
	var fh [frameHeaderLen]byte
	putLeU32(fh[0:4], uint32(len(partial)))
	putLeU32(fh[4:8], 0xdeadbeef)
	f.Write(fh[:])
	f.Write(partial[:len(partial)/2])
	f.Close()

	s2, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("reopen after a torn tail: %v -- a crash must not wedge the spool", err)
	}
	defer s2.Close()
	got, err := s2.Next(100, nil)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("recovered %d records, want the 3 that were intact", len(got))
	}
	for i := range got {
		if got[i].RequestID != batch[i].RequestID {
			t.Errorf("record %d = %s, want %s", i, got[i].RequestID, batch[i].RequestID)
		}
	}
	// The torn bytes are gone, so the next append starts from a clean edge.
	if _, err := s2.Append([]Trace{mkTrace(9)}); err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if _, err := s2.Next(100, nil); err != nil {
		t.Fatalf("Next after recovery: %v", err)
	}
}

func TestDiskSpoolDropsUnreadableSegment(t *testing.T) {
	dir := t.TempDir()
	// A file with the right name and nothing valid in it.
	bad := filepath.Join(dir, fmt.Sprintf("%s%016x%s", segPrefix, 1, segSuffix))
	if err := os.WriteFile(bad, []byte("not a spool segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("open with a junk segment: %v -- one bad file must not wedge metering", err)
	}
	defer s.Close()
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("the unreadable segment was left in place to be re-read forever")
	}
	if _, err := s.Append([]Trace{mkTrace(1)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.Next(10, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("Next = %d, %v; want 1, nil", len(got), err)
	}
}

func TestDiskSpoolIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0o600)
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	s, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if len(s.segs) != 0 {
		t.Errorf("adopted %d unrelated files as segments", len(s.segs))
	}
}

func TestDiskSpoolRecyclesTheActiveSegment(t *testing.T) {
	dir := t.TempDir()
	s, err := openDiskSpool(dir, 1<<20, 1<<20, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Many append/drain cycles with a segment big enough never to roll: without
	// recycling, the active segment would grow to the roll size and the spool
	// would report megabytes held for a queue that is always empty.
	for round := 0; round < 200; round++ {
		if _, err := s.Append([]Trace{mkTrace(round)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		got, err := s.Next(10, nil)
		if err != nil || len(got) != 1 {
			t.Fatalf("round %d: Next = %d, %v", round, len(got), err)
		}
		if err := s.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	_, bytes := s.Depth()
	if bytes > 4<<10 {
		t.Errorf("spool holds %d bytes after 200 drained rounds; the active segment is not recycled", bytes)
	}
}

// --------------------------------------------------------------- mem spool

func TestMemSpoolBoundedAndOrdered(t *testing.T) {
	s := newMemSpool(4 << 10)
	batch := make([]Trace, 200)
	for i := range batch {
		batch[i] = mkTrace(i)
	}
	written, err := s.Append(batch)
	if !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("Append error = %v, want ErrSpoolFull", err)
	}
	if written == 0 || written == len(batch) {
		t.Fatalf("Append accepted %d of %d; want a partial accept", written, len(batch))
	}
	got, err := s.Next(1000, nil)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(got) != written {
		t.Fatalf("read %d, want %d", len(got), written)
	}
	for i := range got {
		if got[i].RequestID != batch[i].RequestID {
			t.Fatalf("record %d out of order", i)
		}
	}
	if recs, _ := s.Depth(); recs != int64(written) {
		t.Errorf("Depth = %d before Commit, want %d", recs, written)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if recs, bytes := s.Depth(); recs != 0 || bytes != 0 {
		t.Errorf("Depth = %d, %d after Commit; want 0, 0", recs, bytes)
	}
}
