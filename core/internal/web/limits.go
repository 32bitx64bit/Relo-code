package web

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	defaultMaxTabs         = 8
	defaultAgentRate       = 60
	defaultGlobalRate      = 240
	defaultDomainInterval  = 1200 * time.Millisecond
	defaultScrollCSSPixels = 900
)

// rateLimiter reserves evenly spaced slots. Reservations made by concurrent
// callers cannot all become due at the same time.
type rateLimiter struct {
	mu        sync.Mutex
	perMinute int
	interval  time.Duration
	next      time.Time
	now       func() time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	l := &rateLimiter{now: time.Now}
	l.setRate(perMinute)
	return l
}

func (l *rateLimiter) setRate(perMinute int) {
	if perMinute <= 0 {
		perMinute = defaultAgentRate
	}
	l.mu.Lock()
	l.perMinute = perMinute
	l.interval = time.Minute / time.Duration(perMinute)
	if l.interval <= 0 {
		l.interval = time.Nanosecond
	}
	l.next = time.Time{}
	l.mu.Unlock()
}

// reserve returns how long the caller must wait for its slot.
func (l *rateLimiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	slot := now
	if l.next.After(now) {
		slot = l.next
	}
	l.next = slot.Add(l.interval)
	return slot.Sub(now)
}

func (l *rateLimiter) wait(ctx context.Context) error {
	return sleepContext(ctx, l.reserve())
}

// domainPacer spaces requests to a registrable domain across every agent. The
// public-suffix fallback keeps IP addresses and unrecognised hosts isolated by
// their exact hostname.
type domainPacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     map[string]time.Time
	now      func() time.Time
}

func newDomainPacer(interval time.Duration) *domainPacer {
	return &domainPacer{interval: interval, next: make(map[string]time.Time), now: time.Now}
}

func domainKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return ""
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err == nil {
		return domain
	}
	return host
}

func (p *domainPacer) reserve(raw string) time.Duration {
	if p == nil || p.interval <= 0 {
		return 0
	}
	key := domainKey(raw)
	if key == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	slot := now
	if p.next[key].After(now) {
		slot = p.next[key]
	}
	p.next[key] = slot.Add(p.interval)
	return slot.Sub(now)
}

func (p *domainPacer) wait(ctx context.Context, raw string) error {
	return sleepContext(ctx, p.reserve(raw))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
