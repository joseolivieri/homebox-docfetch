package discovery

import (
	"context"
	"sync"
	"time"
)

// limiter is a minimal global rate limiter: it spaces calls at least
// interval apart. Zero or negative rate disables limiting.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(perMin int) *limiter {
	if perMin <= 0 {
		return &limiter{}
	}
	return &limiter{interval: time.Minute / time.Duration(perMin)}
}

// wait blocks until the next call slot, or until ctx is done.
func (l *limiter) wait(ctx context.Context) {
	if l.interval == 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	wait := time.Until(l.next)
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
