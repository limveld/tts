package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimerGate(t *testing.T) {
	var lines atomic.Int64
	room := "123"
	chat := &fakeChat{}
	tr := NewTimers(nil, chat, lines.Load, func() string { return room }, log.New(io.Discard, "", 0))
	tc := TimerConfig{Name: "x", Message: "hello", Interval: time.Hour, MinLines: 5}

	last := int64(0)
	var posted bool

	lines.Store(3) // too quiet
	last, posted = tr.tick(tc, last)
	if posted {
		t.Fatal("posted with only 3 lines (< 5)")
	}

	lines.Store(6) // enough
	last, posted = tr.tick(tc, last)
	if !posted || len(chat.sends) != 1 || chat.sends[0] != "hello" {
		t.Fatalf("expected a post; sends=%v posted=%v", chat.sends, posted)
	}

	// Immediately again: no new lines since the last post.
	if _, p := tr.tick(tc, last); p {
		t.Fatal("posted again without new lines")
	}

	// No broadcaster id yet: never posts.
	room = ""
	lines.Store(100)
	if _, p := tr.tick(tc, last); p {
		t.Fatal("posted with no broadcaster id")
	}
}

func TestLoadTimersConfig(t *testing.T) {
	if got, err := LoadTimersConfig(filepath.Join(t.TempDir(), "none.toml")); err != nil || got != nil {
		t.Fatalf("missing file: got=%v err=%v want nil/nil", got, err)
	}

	p := filepath.Join(t.TempDir(), "timers.toml")
	if err := os.WriteFile(p, []byte(`
[[timer]]
name = "discord"
message = "join discord"
interval = "15m"
min_lines = 10
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTimersConfig(p)
	if err != nil || len(got) != 1 {
		t.Fatalf("load: got=%v err=%v", got, err)
	}
	if got[0].Interval != 15*time.Minute || got[0].MinLines != 10 || got[0].Message != "join discord" {
		t.Errorf("timer=%+v", got[0])
	}

	if err := os.WriteFile(p, []byte("[[timer]]\nmessage=\"x\"\ninterval=\"nope\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTimersConfig(p); err == nil {
		t.Error("expected an error for a bad interval")
	}
}
