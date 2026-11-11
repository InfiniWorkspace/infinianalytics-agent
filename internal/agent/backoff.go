package agent

import (
	"math/rand/v2"
	"time"
)

// backoff is capped exponential backoff with ±20 % jitter, so a fleet of
// agents that lost the backend at the same moment does not come back in step.
type backoff struct {
	base, max time.Duration
	attempt   int
	jitter    func() float64 // in [0, 1); replaced in tests
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{base: base, max: max, jitter: rand.Float64}
}

// next is the wait before the next attempt; each call doubles it up to max.
func (b *backoff) next() time.Duration { return b.nextUpTo(b.max) }

// nextUpTo is next with a lower cap for this attempt only. The attempt still
// counts, so a later failure of another kind goes on doubling from there.
func (b *backoff) nextUpTo(limit time.Duration) time.Duration {
	d := b.base << min(b.attempt, 30)
	if d <= 0 || d > b.max {
		d = b.max
	}
	d = min(d, limit)
	b.attempt++
	return time.Duration(float64(d) * (0.8 + 0.4*b.jitter()))
}

// reset starts over after a success.
func (b *backoff) reset() { b.attempt = 0 }
