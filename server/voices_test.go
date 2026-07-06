package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func writeVoices(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "voices.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseVoicesDuplicateCode(t *testing.T) {
	p := writeVoices(t, `
[kokoro]
codes.a = { voice = "af_nicole", weight = 1 }
[polly]
codes.a = { voice = "Brian", weight = 1 }
`)
	if _, err := ParseVoices(p); err == nil {
		t.Fatal("expected an error for a code defined in both sections")
	}
}

func TestParseVoicesMissingVoice(t *testing.T) {
	p := writeVoices(t, "[kokoro]\ncodes.a = { weight = 1 }\n")
	if _, err := ParseVoices(p); err == nil {
		t.Fatal("expected an error for a code with no voice")
	}
}

func TestResolveExplicitAndWeighted(t *testing.T) {
	p := writeVoices(t, `
[kokoro]
codes.a = { voice = "af_nicole", weight = 10 }
codes.b = { voice = "am_adam", weight = 0 }
[polly]
engine = "neural"
codes.k = { voice = "Kevin", weight = 0 }
codes.r = { voice = "Brian", weight = 5 }
`)
	vc, err := ParseVoices(p)
	if err != nil {
		t.Fatal(err)
	}
	vm := vc.Resolver(map[string]bool{"kokoro": true, "polly": true}, rand.New(rand.NewSource(1)))

	// Explicit codes resolve exactly, regardless of weight.
	if e, v := vm.Resolve("k"); e != "polly" || v != "Kevin" {
		t.Errorf("Resolve(k)=%s/%s want polly/Kevin", e, v)
	}
	if e, v := vm.Resolve("b"); e != "kokoro" || v != "am_adam" {
		t.Errorf("Resolve(b)=%s/%s want kokoro/am_adam", e, v)
	}

	// Bare/unknown → weighted-random over weight>0 (a, r); never the weight-0 voices.
	seenA, seenR := false, false
	for i := 0; i < 200; i++ {
		e, v := vm.Resolve("")
		switch v {
		case "af_nicole":
			seenA = true
		case "Brian":
			seenR = true
			if e != "polly" {
				t.Errorf("Brian resolved to engine %q want polly", e)
			}
		default:
			t.Fatalf("random !tts picked %s/%s — a weight-0 voice", e, v)
		}
	}
	if !seenA || !seenR {
		t.Errorf("weighted random missed a weight>0 voice (a=%v r=%v)", seenA, seenR)
	}
}

func TestResolveDisabledPollyFallsBackToKokoro(t *testing.T) {
	p := writeVoices(t, `
[kokoro]
codes.a = { voice = "af_nicole", weight = 10 }
[polly]
codes.k = { voice = "Kevin", weight = 0 }
`)
	vc, _ := ParseVoices(p)
	// Polly not enabled → its codes aren't in the map; !ttsk falls to kokoro random.
	vm := vc.Resolver(map[string]bool{"kokoro": true}, rand.New(rand.NewSource(1)))
	if e, v := vm.Resolve("k"); e != "kokoro" || v != "af_nicole" {
		t.Errorf("Resolve(k) with polly disabled = %s/%s want kokoro/af_nicole", e, v)
	}
}

func TestVoiceTiers(t *testing.T) {
	ev := EngineVoices{
		Engine: "neural",
		Codes: map[string]CodeSpec{
			"r": {Voice: "Brian", Weight: 1},
			"g": {Voice: "Ruth", Weight: 0, Engine: "generative"},
		},
	}
	tiers := ev.VoiceTiers()
	if tiers["Brian"] != "neural" {
		t.Errorf("Brian tier=%q want neural (section default)", tiers["Brian"])
	}
	if tiers["Ruth"] != "generative" {
		t.Errorf("Ruth tier=%q want generative (override)", tiers["Ruth"])
	}
}
