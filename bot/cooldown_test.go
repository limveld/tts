package main

import (
	"testing"
	"time"
)

func TestCooldown(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := NewCooldown(30 * time.Second)
	c.now = func() time.Time { return now }

	if !c.Allow("bob") {
		t.Fatal("first use should be allowed")
	}
	if c.Allow("bob") {
		t.Fatal("immediate second use should be blocked")
	}
	if !c.Allow("alice") {
		t.Fatal("a different user should be allowed")
	}
	now = now.Add(31 * time.Second)
	if !c.Allow("bob") {
		t.Fatal("use after the window should be allowed")
	}
}
