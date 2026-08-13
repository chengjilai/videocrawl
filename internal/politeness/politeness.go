// Package politeness: per-host adaptive rate limiting (techcrawl's model:
// gap = max(minInterval, crawl-delay); failures double the gap up to a cap,
// successes decay it). One limiter per host key; Wait blocks until the gap
// since the host's last request has elapsed.
package politeness

import (
	"sync"
	"time"
)

type hostState struct {
	last   time.Time
	gap    time.Duration
	mu     sync.Mutex
}

type Limiter struct {
	min  time.Duration
	max  time.Duration
	mu   sync.Mutex
	hosts map[string]*hostState
}

func New(minInterval time.Duration) *Limiter {
	return &Limiter{
		min:  minInterval,
		max:  minInterval * 64,
		hosts: map[string]*hostState{},
	}
}

func (l *Limiter) host(key string) *hostState {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, ok := l.hosts[key]
	if !ok {
		h = &hostState{gap: l.min}
		l.hosts[key] = h
	}
	return h
}

// Wait blocks until the host key is due, then records the request time.
func (l *Limiter) Wait(key string) {
	h := l.host(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.last.IsZero() {
		wait := h.gap - time.Since(h.last)
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	h.last = time.Now()
}

// NoteSuccess decays the gap (×0.9, floor at min).
func (l *Limiter) NoteSuccess(key string) {
	h := l.host(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gap > l.min {
		h.gap = time.Duration(float64(h.gap) * 0.9)
		if h.gap < l.min {
			h.gap = l.min
		}
	}
}

// NoteError doubles the gap (cap at max).
func (l *Limiter) NoteError(key string) {
	h := l.host(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gap *= 2
	if h.gap > l.max {
		h.gap = l.max
	}
}

// HostKey returns the politeness key for a URL host.
func HostKey(host string) string { return host }
