package main

import (
	"math/rand"
	"testing"
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestShortCode(t *testing.T) {
	for i, want := range map[int]string{0: "a", 1: "b"} {
		if got := shortCode(i); got != want {
			t.Errorf("shortCode(%d)=%q want %q", i, got, want)
		}
	}
}

func TestDefaultVoiceCodes(t *testing.T) {
	codes := defaultVoiceCodes()
	if len(codes) != len(allVoices) {
		t.Fatalf("got %d codes, want %d", len(codes), len(allVoices))
	}
	if codes["a"] != "af_nicole" {
		t.Errorf(`codes["a"]=%q want af_nicole`, codes["a"])
	}
	if codes["b"] != "am_adam" {
		t.Errorf(`codes["b"]=%q want am_adam`, codes["b"])
	}
}

func TestVoiceResolve(t *testing.T) {
	vr := &VoiceResolver{codes: defaultVoiceCodes(), rnd: rand.New(rand.NewSource(1))}
	if got := vr.Resolve("b"); got != "am_adam" {
		t.Errorf("Resolve(b)=%q want am_adam", got)
	}
	for _, code := range []string{"", "zzz", "9"} { // empty & unknown -> random valid voice
		if got := vr.Resolve(code); !contains(allVoices, got) {
			t.Errorf("Resolve(%q)=%q not a valid voice", code, got)
		}
	}
}
