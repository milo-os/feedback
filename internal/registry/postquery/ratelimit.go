package postquery

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a token bucket per user. It is per replica, so the effective
// limit scales with the number of apiserver replicas.
type Limiter struct {
	every time.Duration
	burst int
	now   func() time.Time

	mu    sync.Mutex
	users map[string]*bucket
	swept time.Time
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

// idle is how long an unused bucket is kept. A bucket idle this long has
// refilled, so dropping it changes nothing.
const idle = 10 * time.Minute

// NewLimiter allows perMinute requests a minute per user, with bursts of
// burst. perMinute <= 0 disables limiting (a nil Limiter allows everything).
func NewLimiter(perMinute float64, burst int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		every: time.Duration(float64(time.Minute) / perMinute),
		burst: burst,
		now:   time.Now,
		users: map[string]*bucket{},
	}
}

// Allow takes a token for user. When none is left it returns false and how
// long until one is.
func (l *Limiter) Allow(user string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > idle {
		for u, b := range l.users {
			if now.Sub(b.seen) > idle {
				delete(l.users, u)
			}
		}
		l.swept = now
	}
	b, ok := l.users[user]
	if !ok {
		b = &bucket{l: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.users[user] = b
	}
	b.seen = now
	r := b.l.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}
