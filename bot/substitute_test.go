package main

import (
	"math/rand"
	"strings"
	"testing"
)

func TestSubstitute(t *testing.T) {
	c := subCtx{User: "Bob", Args: []string{"Alice", "there"}, Rest: "Alice there", Count: 7, rnd: rand.New(rand.NewSource(1))}
	cases := map[string]string{
		"hi $user":           "hi Bob",
		"$touser hello":      "Alice hello",
		"you said: $args":    "you said: Alice there",
		"first=$1 second=$2": "first=Alice second=there",
		"missing=$3":         "missing=",
		"count=$count":       "count=7",
	}
	for in, want := range cases {
		if got := substitute(in, c); got != want {
			t.Errorf("substitute(%q)=%q want %q", in, got, want)
		}
	}

	// $touser falls back to $user with no args.
	if got := substitute("$touser", subCtx{User: "Bob"}); got != "Bob" {
		t.Errorf("$touser no-args=%q want Bob", got)
	}
	// $random is replaced by a number.
	got := substitute("roll $random", subCtx{rnd: rand.New(rand.NewSource(1))})
	if !strings.HasPrefix(got, "roll ") || got == "roll $random" || strings.Contains(got, "$") {
		t.Errorf("$random not substituted: %q", got)
	}
}
