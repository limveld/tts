package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// TimerConfig is one interval announcement. It posts Message every Interval, but
// only if at least MinLines chat messages have arrived since its last post (so it
// doesn't fire into a dead chat).
type TimerConfig struct {
	Name     string
	Message  string
	Interval time.Duration
	MinLines int
}

// LoadTimersConfig parses a timers TOML ([[timer]] entries with a string
// interval like "15m"). A missing file is not an error — timers are opt-in.
func LoadTimersConfig(path string) ([]TimerConfig, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	var doc struct {
		Timer []struct {
			Name     string `toml:"name"`
			Message  string `toml:"message"`
			Interval string `toml:"interval"`
			MinLines int    `toml:"min_lines"`
		} `toml:"timer"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return nil, err
	}
	var out []TimerConfig
	for i, r := range doc.Timer {
		if r.Message == "" {
			return nil, fmt.Errorf("timer %d (%q): message is required", i, r.Name)
		}
		d, err := time.ParseDuration(r.Interval)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("timer %q: invalid interval %q (use e.g. 15m)", r.Name, r.Interval)
		}
		out = append(out, TimerConfig{Name: r.Name, Message: r.Message, Interval: d, MinLines: r.MinLines})
	}
	return out, nil
}

// Timers posts configured interval announcements, gated on chat activity. lines
// returns the running total of chat messages seen; roomID returns the channel's
// broadcaster id ("" until one is known).
type Timers struct {
	cfgs   []TimerConfig
	chat   Chat
	lines  func() int64
	roomID func() string
	logger *log.Logger
}

func NewTimers(cfgs []TimerConfig, chat Chat, lines func() int64, roomID func() string, logger *log.Logger) *Timers {
	return &Timers{cfgs: cfgs, chat: chat, lines: lines, roomID: roomID, logger: logger}
}

// Run drives one ticker per timer until ctx is canceled.
func (t *Timers) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, tc := range t.cfgs {
		wg.Add(1)
		go func(tc TimerConfig) {
			defer wg.Done()
			t.runOne(ctx, tc)
		}(tc)
	}
	wg.Wait()
}

func (t *Timers) runOne(ctx context.Context, tc TimerConfig) {
	ticker := time.NewTicker(tc.Interval)
	defer ticker.Stop()
	last := t.lines()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last, _ = t.tick(tc, last)
		}
	}
}

// tick posts tc.Message if chat has moved at least MinLines since lastLines and a
// broadcaster id is known. It returns the (possibly advanced) line marker and
// whether it posted.
func (t *Timers) tick(tc TimerConfig, lastLines int64) (int64, bool) {
	cur := t.lines()
	if cur-lastLines < int64(tc.MinLines) {
		return lastLines, false // chat too quiet since the last post
	}
	room := t.roomID()
	if room == "" {
		return lastLines, false // no message seen yet, so no broadcaster id
	}
	if err := t.chat.Send(room, tc.Message); err != nil {
		t.logger.Printf("timer %q: %v", tc.Name, err)
		return lastLines, false
	}
	t.logger.Printf("timer %q posted", tc.Name)
	return cur, true
}
