package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// CodeSpec is one chat code's entry in voices.toml. Weight is its relative share
// of the bare-!tts random pool (0 = explicit-only). Engine, when set on a Polly
// entry, overrides the section's default tier for that one voice.
type CodeSpec struct {
	Voice  string `toml:"voice"`
	Weight int    `toml:"weight"`
	Engine string `toml:"engine"`
}

// EngineVoices is one [engine] section of voices.toml.
type EngineVoices struct {
	Engine string              `toml:"engine"` // Polly default tier (ignored for kokoro)
	Region string              `toml:"region"` // Polly region (optional; else AWS default chain)
	Codes  map[string]CodeSpec `toml:"codes"`
}

// VoicesConfig is the parsed voices.toml: one section per engine.
type VoicesConfig struct {
	Kokoro EngineVoices `toml:"kokoro"`
	Polly  EngineVoices `toml:"polly"`
}

// ParseVoices loads and validates voices.toml. Codes must be non-empty and unique
// across all sections (a collision is a config error, whether or not the engine is
// enabled at runtime).
func ParseVoices(path string) (*VoicesConfig, error) {
	var vc VoicesConfig
	if _, err := toml.DecodeFile(path, &vc); err != nil {
		return nil, err
	}
	seen := map[string]string{} // lower(code) -> section
	for _, s := range []struct {
		name string
		sec  EngineVoices
	}{{"kokoro", vc.Kokoro}, {"polly", vc.Polly}} {
		for code, spec := range s.sec.Codes {
			c := strings.ToLower(code)
			if spec.Voice == "" {
				return nil, fmt.Errorf("voices: code %q in [%s] has no voice", code, s.name)
			}
			if other, ok := seen[c]; ok {
				return nil, fmt.Errorf("voices: code %q defined in both [%s] and [%s]", code, other, s.name)
			}
			seen[c] = s.name
		}
	}
	return &vc, nil
}

// VoiceTiers returns a voice-id -> Polly engine tier map for the Polly section,
// each voice defaulting to the section's tier unless it overrides Engine.
func (ev EngineVoices) VoiceTiers() map[string]string {
	tiers := make(map[string]string, len(ev.Codes))
	for _, spec := range ev.Codes {
		tier := spec.Engine
		if tier == "" {
			tier = ev.Engine
		}
		tiers[spec.Voice] = tier
	}
	return tiers
}

// resolvedVoice is one code's routing target.
type resolvedVoice struct {
	engine string
	voice  string
	weight int
}

// VoiceMap resolves chat codes to (engine, voice) for the engines that are
// actually enabled at runtime.
type VoiceMap struct {
	byCode map[string]resolvedVoice
	order  []string // codes sorted, for deterministic weighted selection
	rnd    *rand.Rand
}

// Resolver builds a VoiceMap containing only the codes whose engine is enabled, so
// a code for a disabled engine (e.g. Polly failed to init) falls through to the
// weighted-random pool of the engines that are up.
func (vc *VoicesConfig) Resolver(enabled map[string]bool, rnd *rand.Rand) *VoiceMap {
	m := &VoiceMap{byCode: map[string]resolvedVoice{}, rnd: rnd}
	add := func(engineName string, sec EngineVoices) {
		if !enabled[engineName] {
			return
		}
		for code, spec := range sec.Codes {
			m.byCode[strings.ToLower(code)] = resolvedVoice{engine: engineName, voice: spec.Voice, weight: spec.Weight}
		}
	}
	add("kokoro", vc.Kokoro)
	add("polly", vc.Polly)
	for code := range m.byCode {
		m.order = append(m.order, code)
	}
	sort.Strings(m.order)
	return m
}

// Resolve maps a chat code to (engine, voice). A known code always wins; "" or an
// unknown code returns a weighted-random voice from the enabled pool (voices with
// weight > 0). If nothing has weight, it falls back to any kokoro voice, then any
// voice at all.
func (m *VoiceMap) Resolve(code string) (engine, voice string) {
	if code != "" {
		if e, ok := m.byCode[strings.ToLower(code)]; ok {
			return e.engine, e.voice
		}
	}
	total := 0
	for _, e := range m.byCode {
		if e.weight > 0 {
			total += e.weight
		}
	}
	if total > 0 {
		n := m.rnd.Intn(total)
		for _, code := range m.order {
			e := m.byCode[code]
			if e.weight <= 0 {
				continue
			}
			if n < e.weight {
				return e.engine, e.voice
			}
			n -= e.weight
		}
	}
	// No weighted pool: prefer any kokoro voice, else any voice, else empty (the
	// engine falls back to its own default).
	for _, code := range m.order {
		if e := m.byCode[code]; e.engine == "kokoro" {
			return e.engine, e.voice
		}
	}
	for _, code := range m.order {
		e := m.byCode[code]
		return e.engine, e.voice
	}
	return "kokoro", ""
}
