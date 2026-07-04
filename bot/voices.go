package main

import (
	"math/rand"
	"strings"
)

// allVoices is the full Kokoro voice list, in the order used to assign default
// short codes (see kokoro/demo/app.py CHOICES). Codes are a, b, c, … by index.
var allVoices = []string{
	//"af_heart",
	//"af_bella",
	"af_nicole",
	//"af_aoede",
	//"af_kore",
	//"af_sarah",

	//"af_nova",
	//"af_sky",
	//"af_alloy",
	//"af_jessica",
	//"af_river",

	//"am_michael",
	//"am_fenrir",
	//"am_puck",
	//"am_echo",
	//"am_eric",
	//"am_liam",
	//
	//"am_onyx",
	//"am_santa",
	"am_adam",

	//"bf_emma",
	//"bf_isabella",
	//"bf_alice",
	//"bf_lily",

	//"bm_george",
	//"bm_fable",
	//"bm_lewis",
	//"bm_daniel",
}

// shortCode returns the default chat code for the i-th voice: a…z for 0–25,
// then aa, ab, … for 26+.
func shortCode(i int) string {
	if i < 2 {
		return string(rune('a' + i))
	}
	return "a" + string(rune('a'+(i-2)))
}

// defaultVoiceCodes builds the default code→voiceID map.
func defaultVoiceCodes() map[string]string {
	codes := make(map[string]string, len(allVoices))
	for i, v := range allVoices {
		codes[shortCode(i)] = v
	}
	return codes
}

// VoiceResolver maps a chat command's voice code to a Kokoro voice id, falling
// back to a random voice for the empty (=!tts) or unknown codes.
type VoiceResolver struct {
	codes map[string]string
	rnd   *rand.Rand
}

// Resolve returns the voice id for a code. "" (bare !tts) and unknown codes
// both resolve to a random voice.
func (vr *VoiceResolver) Resolve(code string) string {
	if code != "" {
		if v, ok := vr.codes[strings.ToLower(code)]; ok {
			return v
		}
	}
	return allVoices[vr.rnd.Intn(len(allVoices))]
}
