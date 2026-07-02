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
	for i, want := range map[int]string{0: "a", 25: "z", 26: "aa", 27: "ab"} {
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
	if codes["b"] != "af_bella" {
		t.Errorf(`codes["b"]=%q want af_bella`, codes["b"])
	}
	if codes["ab"] != "bm_daniel" {
		t.Errorf(`codes["ab"]=%q want bm_daniel`, codes["ab"])
	}
}

func TestVoiceResolve(t *testing.T) {
	vr := &VoiceResolver{codes: defaultVoiceCodes(), rnd: rand.New(rand.NewSource(1))}
	if got := vr.Resolve("b"); got != "af_bella" {
		t.Errorf("Resolve(b)=%q want af_bella", got)
	}
	for _, code := range []string{"", "zzz", "9"} { // empty & unknown -> random valid voice
		if got := vr.Resolve(code); !contains(allVoices, got) {
			t.Errorf("Resolve(%q)=%q not a valid voice", code, got)
		}
	}
}
