package bridgeapp

import (
	"bytes"
	"testing"
	"time"
)

func TestBackoffMonotoneInAttemptUpToCap(t *testing.T) {
	b := Backoff{Base: 5 * time.Millisecond, Max: 300 * time.Millisecond, Jitter: 0}

	want := []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
		300 * time.Millisecond, // base*2^6 = 320ms exceeds the cap
		300 * time.Millisecond,
	}
	for attempt, expected := range want {
		if got := b.Next(attempt, nil); got != expected {
			t.Fatalf("Next(%d) = %s, want %s", attempt, got, expected)
		}
	}

	// Monotone and capped far past the doubling range.
	var previous time.Duration
	for attempt := 0; attempt < 32; attempt++ {
		got := b.Next(attempt, nil)
		if attempt > 0 && got < previous {
			t.Fatalf("Next(%d) = %s shrank below %s", attempt, got, previous)
		}
		if got > b.Max {
			t.Fatalf("Next(%d) = %s exceeds cap %s", attempt, got, b.Max)
		}
		previous = got
	}
	if previous != b.Max {
		t.Fatalf("Next at large attempt = %s, want the cap %s", previous, b.Max)
	}

	// Negative attempts clamp to the first retry.
	if got := b.Next(-7, nil); got != 5*time.Millisecond {
		t.Fatalf("Next(-7) = %s, want 5ms", got)
	}

	// The cap also bounds the total when jitter alone would exceed it.
	tight := Backoff{Base: time.Second, Max: 100 * time.Millisecond, Jitter: 250 * time.Millisecond}
	if got := tight.Next(0, bytes.NewReader(make([]byte, 16))); got != 100*time.Millisecond {
		t.Fatalf("Next with Max below jitter = %s, want the 100ms cap", got)
	}
}

func TestBackoffJitterDerivedFromRandomAndDeterministic(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 10 * time.Second, Jitter: time.Second}

	// An all-zero reader derives the minimum jitter: exactly the base part.
	if got := b.Next(0, bytes.NewReader(make([]byte, 128))); got != time.Second {
		t.Fatalf("Next with zero reader = %s, want exactly 1s (no jitter)", got)
	}
	if got := b.Next(3, bytes.NewReader(make([]byte, 128))); got != 8*time.Second {
		t.Fatalf("Next(3) with zero reader = %s, want exactly 8s (no jitter)", got)
	}

	// An all-0xff reader derives near-maximum jitter, still under Max.
	peaked := b.Next(0, bytes.NewReader(bytes.Repeat([]byte{0xff}, 128)))
	if peaked <= time.Second || peaked >= 2*time.Second {
		t.Fatalf("Next with 0xff reader = %s, want within (1s, 2s)", peaked)
	}

	// Jitter is derived from the reader, so identical readers produce
	// identical sequences (deterministic under a fixed reader).
	pattern := []byte{0x27, 0x5b, 0x91, 0xc3, 0x6a, 0xf0, 0x11, 0x04}
	first := backoffSequence(t, b, 6, pattern)
	second := backoffSequence(t, b, 6, pattern)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("sequence diverged at %d: %v vs %v", i, first, second)
		}
	}
	for i, got := range first {
		// Base 1s doubles per attempt: 1s, 2s, 4s, 8s; from attempt 4 the
		// exponential part (16s) exceeds the jitter-free room (Max-Jitter =
		// 9s) and is pinned there, so only the jitter varies.
		wantBase := time.Duration(1<<uint(i)) * time.Second
		if i >= 4 {
			wantBase = 9 * time.Second
		}
		if got < wantBase || got >= wantBase+time.Second {
			t.Fatalf("sequence[%d] = %s, want within [%s, %s)", i, got, wantBase, wantBase+time.Second)
		}
	}
}

func TestBackoffZeroJitterDoesNotReadRandom(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 8 * time.Second, Jitter: 0}
	reader := &countingReader{}
	if got := b.Next(4, reader); got != 8*time.Second {
		t.Fatalf("Next(4) = %s, want the 8s cap", got)
	}
	if reader.reads != 0 {
		t.Fatalf("jitterless Next read from random %d times, want 0", reader.reads)
	}
}

func backoffSequence(t *testing.T, b Backoff, attempts int, pattern []byte) []time.Duration {
	t.Helper()
	reader := bytes.NewReader(bytes.Repeat(pattern, 1+attempts))
	delays := make([]time.Duration, 0, attempts)
	for attempt := 0; attempt < attempts; attempt++ {
		delays = append(delays, b.Next(attempt, reader))
	}
	return delays
}

type countingReader struct{ reads int }

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}
