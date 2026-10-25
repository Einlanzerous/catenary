package linkpreview

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Limiter is a per-account token bucket: perMinute tokens a minute, burst at
// most. It is local to the link preview endpoint on purpose. store.ErrRateLimited
// is a wire code with no policy behind it, and a general limiter would be a
// decision for every route; this one bounds only how fast a member can make the
// server open sockets for them.
type Limiter struct {
	mu       sync.Mutex
	rate     float64 // tokens per second
	burst    float64
	now      func() time.Time
	accounts map[uuid.UUID]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxIdleAccounts is when Allow starts dropping buckets that have refilled: a
// full bucket is the same as no bucket, so pruning never changes an answer.
const maxIdleAccounts = 1024

// NewLimiter returns a limiter. now is time.Now unless a test supplies a clock.
func NewLimiter(perMinute, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		rate:     float64(perMinute) / 60,
		burst:    float64(burst),
		now:      now,
		accounts: map[uuid.UUID]*bucket{},
	}
}

// Allow takes one token for account. When there is none it returns how long
// until there is one.
func (l *Limiter) Allow(account uuid.UUID) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.accounts[account]
	if b == nil {
		if len(l.accounts) >= maxIdleAccounts {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.accounts[account] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func (l *Limiter) prune(now time.Time) {
	for id, b := range l.accounts {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.accounts, id)
		}
	}
}
