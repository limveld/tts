package main

import (
	"sync"
	"time"
)

// Cooldown enforces a per-user minimum interval between actions.
type Cooldown struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time
	now    func() time.Time // injectable for tests
}

func NewCooldown(window time.Duration) *Cooldown {
	return &Cooldown{window: window, last: make(map[string]time.Time), now: time.Now}
}

// Allow reports whether user may act now; if so, it records the time.
func (c *Cooldown) Allow(user string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now()
	if prev, ok := c.last[user]; ok && t.Sub(prev) < c.window {
		return false
	}
	c.last[user] = t
	return true
}

// Remaining returns how long until user may act again (0 if they're free now).
func (c *Cooldown) Remaining(user string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.last[user]; ok {
		if d := c.window - c.now().Sub(prev); d > 0 {
			return d
		}
	}
	return 0
}
