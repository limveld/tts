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

func TestCooldownRemaining(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := NewCooldown(30 * time.Second)
	c.now = func() time.Time { return now }

	if d := c.Remaining("bob"); d != 0 {
		t.Fatalf("never-used Remaining=%v want 0", d)
	}
	c.Allow("bob")
	if d := c.Remaining("bob"); d != 30*time.Second {
		t.Fatalf("just-used Remaining=%v want 30s", d)
	}
	now = now.Add(20 * time.Second)
	if d := c.Remaining("bob"); d != 10*time.Second {
		t.Fatalf("Remaining=%v want 10s", d)
	}
	now = now.Add(15 * time.Second)
	if d := c.Remaining("bob"); d != 0 {
		t.Fatalf("past-window Remaining=%v want 0", d)
	}
}
