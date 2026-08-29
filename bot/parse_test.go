package main

import (
	"testing"
	"time"
)

func TestParsePrivmsg(t *testing.T) {
	line := `@badge-info=;badges=moderator/1;display-name=Bob;emotes=;mod=1;subscriber=0;vip=0 :bob!bob@bob.tmi.twitch.tv PRIVMSG #streamer :!tts hello world`
	m, ok := parsePrivmsg(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if m.User != "bob" || m.Display != "Bob" || m.Channel != "streamer" {
		t.Errorf("user/display/channel = %q/%q/%q", m.User, m.Display, m.Channel)
	}
	if m.Text != "!tts hello world" {
		t.Errorf("text = %q", m.Text)
	}
	if !m.IsMod || m.IsBroadcaster {
		t.Errorf("mod=%v broadcaster=%v", m.IsMod, m.IsBroadcaster)
	}
}

func TestParseCapturesReplyTags(t *testing.T) {
	line := `@id=abc-123;user-id=555;room-id=999;display-name=Bob;mod=0 :bob!bob@bob.tmi.twitch.tv PRIVMSG #streamer :!sfx`
	m, ok := parsePrivmsg(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if m.ID != "abc-123" || m.UserID != "555" || m.RoomID != "999" {
		t.Errorf("id/user-id/room-id = %q/%q/%q", m.ID, m.UserID, m.RoomID)
	}
}

func TestParseBroadcaster(t *testing.T) {
	line := `@badges=broadcaster/1;display-name=Streamer;mod=0 :streamer!streamer@streamer.tmi.twitch.tv PRIVMSG #streamer :!skip`
	m, ok := parsePrivmsg(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if !m.IsBroadcaster || !m.IsMod { // broadcaster implies mod
		t.Errorf("broadcaster=%v mod=%v", m.IsBroadcaster, m.IsMod)
	}
}

func TestParseNonPrivmsg(t *testing.T) {
	for _, line := range []string{
		"PING :tmi.twitch.tv",
		":tmi.twitch.tv 001 justinfan123 :Welcome, GLHF!",
		"",
	} {
		if _, ok := parsePrivmsg(line); ok {
			t.Errorf("expected not ok for %q", line)
		}
	}
}

func TestParseClearmsg(t *testing.T) {
	line := `@login=bob;room-id=999;target-msg-id=abc-123;tmi-sent-ts=1700000000500 :tmi.twitch.tv CLEARMSG #streamer :the offending text`
	d, ok := parseClearmsg(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if d.MsgID != "abc-123" || d.RoomID != "999" {
		t.Errorf("msg-id/room-id = %q/%q", d.MsgID, d.RoomID)
	}
	// tmi-sent-ts is milliseconds; the store keeps unix seconds.
	if d.At != 1700000000 {
		t.Errorf("at = %d want 1700000000 (millis truncated to seconds)", d.At)
	}
}

// Twitch does not reliably populate room-id on CLEARMSG. An empty value is left
// empty rather than guessed at, so the caller can substitute the room it has
// been tracking; inventing one here would silently tombstone nothing.
func TestParseClearmsgWithoutRoomID(t *testing.T) {
	d, ok := parseClearmsg(`@login=bob;target-msg-id=abc-123 :tmi.twitch.tv CLEARMSG #streamer :text`)
	if !ok {
		t.Fatal("expected ok")
	}
	if d.RoomID != "" {
		t.Errorf("room-id = %q want empty", d.RoomID)
	}
	// A missing tmi-sent-ts falls back to now rather than the epoch, which would
	// place the deletion before every message it could possibly describe.
	if d.At < time.Now().Unix()-5 {
		t.Errorf("at = %d want ~now", d.At)
	}
}

func TestParseClearchat(t *testing.T) {
	line := `@ban-duration=600;room-id=999;target-user-id=555;tmi-sent-ts=1700000000500 :tmi.twitch.tv CLEARCHAT #streamer :bob`
	c, ok := parseClearchat(line)
	if !ok {
		t.Fatal("expected ok")
	}
	if c.UserID != "555" || c.RoomID != "999" || c.At != 1700000000 {
		t.Errorf("user-id/room-id/at = %q/%q/%d", c.UserID, c.RoomID, c.At)
	}

	// A permanent ban carries no ban-duration and is otherwise identical.
	perm, ok := parseClearchat(`@room-id=999;target-user-id=555;tmi-sent-ts=1700000000500 :tmi.twitch.tv CLEARCHAT #streamer :bob`)
	if !ok || perm.UserID != "555" {
		t.Errorf("permanent ban: ok=%v user-id=%q", ok, perm.UserID)
	}
}

// A CLEARCHAT with no target-user-id is a mod running /clear to tidy the display.
// Treating it as a tombstone would erase the whole channel's history on a routine
// action, so it is deliberately not one.
func TestParseClearchatWholeChannelIsIgnored(t *testing.T) {
	if c, ok := parseClearchat(`@room-id=999;tmi-sent-ts=1700000000500 :tmi.twitch.tv CLEARCHAT #streamer`); ok {
		t.Errorf("whole-channel clear parsed as a tombstone: %+v", c)
	}
}

// Each parser answers only for its own command, so the read loop can try them in
// turn without one swallowing another's lines.
func TestTombstoneParsersRejectOtherCommands(t *testing.T) {
	privmsg := `@id=abc;user-id=555;room-id=999 :bob!bob@bob.tmi.twitch.tv PRIVMSG #streamer :hi`
	clearmsg := `@target-msg-id=abc :tmi.twitch.tv CLEARMSG #streamer :hi`
	clearchat := `@target-user-id=555 :tmi.twitch.tv CLEARCHAT #streamer :bob`

	if _, ok := parseClearmsg(privmsg); ok {
		t.Error("parseClearmsg accepted a PRIVMSG")
	}
	if _, ok := parseClearchat(privmsg); ok {
		t.Error("parseClearchat accepted a PRIVMSG")
	}
	if _, ok := parseClearmsg(clearchat); ok {
		t.Error("parseClearmsg accepted a CLEARCHAT")
	}
	if _, ok := parseClearchat(clearmsg); ok {
		t.Error("parseClearchat accepted a CLEARMSG")
	}
	if _, ok := parsePrivmsg(clearmsg); ok {
		t.Error("parsePrivmsg accepted a CLEARMSG")
	}
}
