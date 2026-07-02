package main

import "testing"

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
