package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	urlRe   = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	spaceRe = regexp.MustCompile(`\s+`)
)

func isNotWord(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }

// removeEmotes strips the character ranges named by Twitch's `emotes` tag from
// text. Positions are code-point indices into the full message, so this must be
// applied to the whole message before any other edit.
//
// emotes format: "25:0-4,12-16/1902:6-10".
func removeEmotes(text, emotes string) string {
	if emotes == "" {
		return text
	}
	runes := []rune(text)
	remove := make([]bool, len(runes))
	for _, group := range strings.Split(emotes, "/") {
		c := strings.IndexByte(group, ':')
		if c < 0 {
			continue
		}
		for _, rng := range strings.Split(group[c+1:], ",") {
			d := strings.IndexByte(rng, '-')
			if d < 0 {
				continue
			}
			start, err1 := strconv.Atoi(rng[:d])
			end, err2 := strconv.Atoi(rng[d+1:])
			if err1 != nil || err2 != nil {
				continue
			}
			for p := start; p >= 0 && p <= end && p < len(runes); p++ {
				remove[p] = true
			}
		}
	}
	var b strings.Builder
	for i, r := range runes {
		if !remove[i] {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// collapseRepeats squashes runs of the same character (to at most 10) and drops
// consecutive duplicate words, keeping clips short and legible.
func collapseRepeats(text string) string {
	var b strings.Builder
	var last rune
	run, first := 0, true
	for _, r := range text {
		if !first && r == last {
			run++
		} else {
			run, last, first = 1, r, false
		}
		if run <= 10 {
			b.WriteRune(r)
		}
	}
	s := spaceRe.ReplaceAllString(b.String(), " ")

	words := strings.Fields(s)
	out := make([]string, 0, len(words))
	for i, w := range words {
		if i > 0 && strings.EqualFold(w, words[i-1]) {
			continue
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// containsBlocked reports whether text contains any blocklist term. Single-word
// terms match on word boundaries; multi-word terms match as substrings.
func containsBlocked(text string, blocklist []string) bool {
	if len(blocklist) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	padded := " " + strings.Join(strings.FieldsFunc(lower, isNotWord), " ") + " "
	for _, term := range blocklist {
		t := strings.ToLower(strings.TrimSpace(term))
		switch {
		case t == "":
			continue
		case strings.Contains(t, " "):
			if strings.Contains(lower, t) {
				return true
			}
		default:
			if strings.Contains(padded, " "+t+" ") {
				return true
			}
		}
	}
	return false
}

// Clean prepares a message for TTS: strip URLs, collapse spam, reject blocked or
// empty text, and cap the length. Emote removal is applied earlier, to the full
// message (see removeEmotes). ok is false when the message should be dropped.
func Clean(text string, blocklist []string, maxChars int) (string, bool) {
	text = urlRe.ReplaceAllString(text, " ")
	text = strings.TrimSpace(collapseRepeats(text))
	if text == "" || containsBlocked(text, blocklist) {
		return "", false
	}
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		text = strings.TrimSpace(string([]rune(text)[:maxChars]))
	}
	if text == "" {
		return "", false
	}
	return text, true
}
