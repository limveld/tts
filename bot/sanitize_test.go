package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRemoveEmotes(t *testing.T) {
	// "!tts Kappa hi": Kappa occupies code points 5-9.
	if got := removeEmotes("!tts Kappa hi", "25:5-9"); got != "!tts  hi" {
		t.Errorf("removeEmotes = %q want %q", got, "!tts  hi")
	}
	if got := removeEmotes("no emotes", ""); got != "no emotes" {
		t.Errorf("removeEmotes(no tag) = %q", got)
	}
}

func TestCleanStripsURL(t *testing.T) {
	got, ok := Clean("check https://evil.example/x out", nil, 200)
	if !ok {
		t.Fatal("expected ok")
	}
	if strings.Contains(got, "http") || strings.Contains(got, "evil") {
		t.Errorf("url not stripped: %q", got)
	}
}

func TestCleanCollapse(t *testing.T) {
	// Character runs are capped at 10.
	if got, _ := Clean("heyyyyyyyyyyyyyy", nil, 200); got != "heyyyyyyyyyy" {
		t.Errorf("char collapse = %q want %q", got, "heyyyyyyyyyy")
	}
	// Consecutive duplicate words are capped at 10, too.
	want := strings.TrimSpace(strings.Repeat("lol ", 10))
	if got, _ := Clean(strings.Repeat("lol ", 15), nil, 200); got != want {
		t.Errorf("word collapse = %q want 10x lol", got)
	}
	// Under the limits, text is left intact.
	if got, _ := Clean("heyyy heyyy lol", nil, 200); got != "heyyy heyyy lol" {
		t.Errorf("collapse = %q want unchanged", got)
	}
}

func TestCleanBlocklist(t *testing.T) {
	if _, ok := Clean("this is badword here", []string{"badword"}, 200); ok {
		t.Error("expected blocked")
	}
	if _, ok := Clean("this is totally fine", []string{"badword"}, 200); !ok {
		t.Error("expected allowed")
	}
}

func TestCleanDropsEmpty(t *testing.T) {
	if _, ok := Clean("    ", nil, 200); ok {
		t.Error("expected drop for whitespace")
	}
	if _, ok := Clean("https://only.link", nil, 200); ok {
		t.Error("expected drop for url-only")
	}
}

func TestCleanLengthCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "w%d ", i)
	}
	got, ok := Clean(sb.String(), nil, 20)
	if !ok {
		t.Fatal("expected ok")
	}
	if n := utf8.RuneCountInString(got); n > 20 {
		t.Errorf("len %d > 20: %q", n, got)
	}
}
