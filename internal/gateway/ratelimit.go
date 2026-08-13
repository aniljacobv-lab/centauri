package gateway

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-window (per-minute) counter. The whole map is reset
// when the window rolls over, so memory is bounded by the number of distinct
// callers seen within one minute — no background sweeper needed.
type rateLimiter struct {
	mu     sync.Mutex
	window int64 // unix minute the counts belong to
	counts map[string]int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{counts: map[string]int{}}
}

// allow counts one request against key and reports whether it fits within
// limit for the current minute. When rejected it also returns the seconds
// until the window resets (Retry-After).
func (rl *rateLimiter) allow(key string, limit int, now time.Time) (bool, int) {
	if limit <= 0 {
		return true, 0 // unlimited
	}
	win := now.Unix() / 60
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if win != rl.window {
		rl.window = win
		rl.counts = map[string]int{}
	}
	rl.counts[key]++
	if rl.counts[key] > limit {
		retry := int(60 - now.Unix()%60)
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	return true, 0
}
