package k8s

import (
	"context"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// A log line must never be split by a read boundary. Before this was fixed the
// reader consumed fixed 1024-byte chunks, so any line crossing a chunk edge lost
// its kubelet timestamp prefix on the tail fragment — mangling the message and
// falling back to a non-deterministic time.Now(), which breaks dedup.
// iotest.OneByteReader is the worst case: a boundary between every single byte.
func TestStreamLogEntries_LinesSurviveReadBoundaries(t *testing.T) {
	long := strings.Repeat("x", 5000) // far larger than any single read
	raw := "2026-07-12T10:00:00.000000001Z first line\n" +
		"2026-07-12T10:00:01.000000002Z " + long + "\n" +
		"2026-07-12T10:00:02.000000003Z last line without newline"

	logChan := make(chan LogEntry, 16)
	err := streamLogEntries(
		context.Background(),
		iotest.OneByteReader(strings.NewReader(raw)),
		"dkjr", "pod-1", "app", logChan,
	)
	if err != nil {
		t.Fatalf("streamLogEntries: %v", err)
	}
	close(logChan)

	got := make([]LogEntry, 0, 3)
	for e := range logChan {
		got = append(got, e)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}

	if got[0].Message != "first line" {
		t.Errorf("entry 0 message = %q", got[0].Message)
	}
	if got[1].Message != long {
		t.Errorf("entry 1 split or truncated: len=%d, want %d", len(got[1].Message), len(long))
	}
	// A trailing line with no newline must still be emitted.
	if got[2].Message != "last line without newline" {
		t.Errorf("entry 2 message = %q", got[2].Message)
	}

	// Every line kept its real kubelet timestamp — none fell back to time.Now().
	want := []time.Time{
		time.Date(2026, 7, 12, 10, 0, 0, 1, time.UTC),
		time.Date(2026, 7, 12, 10, 0, 1, 2, time.UTC),
		time.Date(2026, 7, 12, 10, 0, 2, 3, time.UTC),
	}
	for i, w := range want {
		if !got[i].Timestamp.Equal(w) {
			t.Errorf("entry %d timestamp = %v, want %v", i, got[i].Timestamp, w)
		}
	}
}

// Identical messages at different real times must stay distinct (separate rows),
// while the same line re-read must reproduce the same timestamp (dedupes).
func TestStreamLogEntries_RepeatedMessageKeepsDistinctTimestamps(t *testing.T) {
	raw := "2026-07-12T10:00:00.000000000Z /health 200 OK\n" +
		"2026-07-12T10:00:05.000000000Z /health 200 OK\n"

	read := func() []LogEntry {
		ch := make(chan LogEntry, 8)
		if err := streamLogEntries(
			context.Background(), strings.NewReader(raw), "dkjr", "pod-1", "app", ch,
		); err != nil {
			t.Fatalf("streamLogEntries: %v", err)
		}
		close(ch)
		out := make([]LogEntry, 0, 2)
		for e := range ch {
			out = append(out, e)
		}
		return out
	}

	first, second := read(), read()
	if len(first) != 2 {
		t.Fatalf("want 2 entries, got %d", len(first))
	}
	if first[0].Timestamp.Equal(first[1].Timestamp) {
		t.Error("distinct occurrences collapsed to one timestamp")
	}
	// Re-reading the same stream (as happens on an overlapping window or a worker
	// restart) must yield identical timestamps so the rows dedupe.
	for i := range first {
		if !first[i].Timestamp.Equal(second[i].Timestamp) {
			t.Errorf("entry %d timestamp not stable across re-reads: %v vs %v",
				i, first[i].Timestamp, second[i].Timestamp)
		}
	}
}

func TestStreamLogEntries_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := streamLogEntries(ctx, strings.NewReader("2026-07-12T10:00:00Z x\n"),
		"dkjr", "pod-1", "app", make(chan LogEntry, 1))
	if err == nil {
		t.Error("want context error, got nil")
	}
}
