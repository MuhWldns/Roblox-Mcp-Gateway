package bridgeapp

import (
	"encoding/binary"
	"io"
	"math"
	"time"
)

// Backoff is the capped exponential delay used between Bridge reconnect
// attempts. The exponential part doubles per attempt and stops growing so the
// jittered total never exceeds Max.
type Backoff struct {
	// Base is the delay before the first retry.
	Base time.Duration
	// Max caps the total returned delay, jitter included.
	Max time.Duration
	// Jitter is the upper bound of the non-negative pseudo-random addition
	// read from the caller-supplied reader.
	Jitter time.Duration
}

// Next returns the reconnect delay after the given attempt, 0 being the first
// retry. The exponential part is min(Base shifted by the attempt, Max-Jitter);
// when Jitter is positive, eight bytes are read from random to derive a
// deterministic delay in [0, Jitter), so identical readers produce identical
// delays. A nil or failing reader simply yields no jitter, and a Jitter of
// zero never reads from random at all.
func (b Backoff) Next(attempt int, random io.Reader) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := clampNonNegative(b.Base)
	max := clampNonNegative(b.Max)
	jitter := clampNonNegative(b.Jitter)
	if max <= jitter {
		// The jitter alone would exceed the cap; never return more than Max.
		return max
	}
	room := max - jitter
	delay := base
	for i := 0; i < attempt && delay < room; i++ {
		if delay > math.MaxInt64/2 {
			delay = room
			break
		}
		delay *= 2
	}
	if delay > room {
		delay = room
	}
	return delay + jitterFraction(jitter, random)
}

func clampNonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// jitterFraction maps eight bytes from random onto a fraction of jitter. The
// 53-bit fraction mirrors IEEE 754 double precision, so any reader content
// yields a delay strictly below jitter.
func jitterFraction(jitter time.Duration, random io.Reader) time.Duration {
	if jitter <= 0 || random == nil {
		return 0
	}
	var seed [8]byte
	if _, err := io.ReadFull(random, seed[:]); err != nil {
		return 0
	}
	fraction := float64(binary.BigEndian.Uint64(seed[:])&(1<<53-1)) / (1 << 53)
	return time.Duration(fraction * float64(jitter))
}
