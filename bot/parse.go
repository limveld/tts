package main

import (
	"strconv"
	"strings"
	"time"
)

// ChatMessage is a parsed Twitch PRIVMSG.
type ChatMessage struct {
	User          string // login, lowercased
	Display       string
	Channel       string
	Text          string
	IsMod         bool
	IsBroadcaster bool
	IsSub         bool
	IsVIP         bool
	Emotes        string // raw IRC `emotes` tag (positions into Text)
	ID            string // message id (`id` tag) — for reply_parent_message_id
	UserID        string // sender's user id (`user-id` tag) — stable per-user key
	RoomID        string // channel/broadcaster id (`room-id` tag) — Helix broadcaster_id
}

// ircLine is one IRCv3 line split into the parts a handler needs. Tag parsing
// and the prefix/command/params split are identical for every message type the
// bot reads, so they live here rather than being repeated in each parser.
type ircLine struct {
	tags    map[string]string
	prefix  string // "nick!user@host", or "tmi.twitch.tv" on server-sent commands
	command string // PRIVMSG, CLEARMSG, CLEARCHAT, …
	params  string // everything after the command word
}

func parseIRCLine(line string) (ircLine, bool) {
	l := ircLine{tags: map[string]string{}}
	rest := line

	if strings.HasPrefix(rest, "@") {
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			return l, false
		}
		for _, kv := range strings.Split(rest[1:sp], ";") {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				l.tags[kv[:i]] = kv[i+1:]
			}
		}
		rest = rest[sp+1:]
	}

	// prefix: :nick!user@host on a PRIVMSG, :tmi.twitch.tv on a tombstone.
	if !strings.HasPrefix(rest, ":") {
		return l, false
	}
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return l, false
	}
	l.prefix = rest[1:sp]
	rest = rest[sp+1:]

	if sp = strings.IndexByte(rest, ' '); sp < 0 {
		l.command = rest
		return l, true
	}
	l.command, l.params = rest[:sp], rest[sp+1:]
	return l, true
}

// parsePrivmsg parses one raw IRCv3 line. ok is false for non-PRIVMSG lines.
//
// Example line:
//
//	@badges=moderator/1;display-name=Bob;emotes=25:5-9;mod=1;subscriber=0 :bob!bob@bob.tmi.twitch.tv PRIVMSG #chan :!tts Kappa hi
func parsePrivmsg(line string) (ChatMessage, bool) {
	var m ChatMessage
	l, ok := parseIRCLine(line)
	if !ok || l.command != "PRIVMSG" {
		return m, false
	}
	tags := l.tags

	// params: #channel :message
	sp := strings.IndexByte(l.params, ' ')
	if sp < 0 {
		return m, false
	}
	channel := strings.TrimPrefix(l.params[:sp], "#")
	text := strings.TrimPrefix(l.params[sp+1:], ":")

	login := l.prefix
	if i := strings.IndexByte(login, '!'); i >= 0 {
		login = login[:i]
	}

	m.User = strings.ToLower(login)
	m.Channel = channel
	m.Text = strings.TrimRight(text, "\r\n")
	m.Display = tags["display-name"]
	if m.Display == "" {
		m.Display = login
	}
	m.IsMod = tags["mod"] == "1"
	m.IsSub = tags["subscriber"] == "1"
	m.IsVIP = tags["vip"] == "1"
	m.Emotes = tags["emotes"]
	m.ID = tags["id"]
	m.UserID = tags["user-id"]
	m.RoomID = tags["room-id"]
	m.IsBroadcaster = strings.Contains(tags["badges"], "broadcaster/") || m.User == strings.ToLower(channel)
	if m.IsBroadcaster {
		m.IsMod = true // broadcaster is implicitly a mod
	}
	return m, true
}

// ChatDelete is a CLEARMSG: a moderator removed one specific message.
//
// Example line:
//
//	@login=bob;room-id=1;target-msg-id=abc-123;tmi-sent-ts=1700000000000 :tmi.twitch.tv CLEARMSG #chan :the offending text
type ChatDelete struct {
	// RoomID comes from the room-id tag, which Twitch does not always populate on
	// CLEARMSG. An empty value is left empty here rather than guessed at; the
	// caller substitutes the room it has been tracking from PRIVMSG tags.
	RoomID string
	MsgID  string // target-msg-id: the message that was removed
	At     int64  // unix seconds
}

// parseClearmsg parses a CLEARMSG line. ok is false for anything else, and for a
// CLEARMSG carrying no target-msg-id — there would be nothing to tombstone.
func parseClearmsg(line string) (ChatDelete, bool) {
	var d ChatDelete
	l, ok := parseIRCLine(line)
	if !ok || l.command != "CLEARMSG" {
		return d, false
	}
	if d.MsgID = l.tags["target-msg-id"]; d.MsgID == "" {
		return d, false
	}
	d.RoomID = l.tags["room-id"]
	d.At = tmiSeconds(l.tags["tmi-sent-ts"])
	return d, true
}

// ChatClear is a CLEARCHAT: a ban or a timeout against one user.
//
// Example line:
//
//	@ban-duration=600;room-id=1;target-user-id=99;tmi-sent-ts=1700000000000 :tmi.twitch.tv CLEARCHAT #chan :bob
//
// A CLEARCHAT with no target-user-id is a whole-channel wipe (a mod running
// /clear), and parseClearchat reports ok=false for it on purpose. /clear tidies
// what is on screen and says nothing about the messages themselves; tombstoning
// a channel's entire history because someone cleared the display would erase the
// distinction the log exists to record.
//
// The ban-duration tag — present for a timeout, absent for a permanent ban — is
// deliberately not carried. A tombstone records that the line was removed by a
// CLEARCHAT; nothing downstream distinguishes the two, and an unread field is a
// question about why it is there.
type ChatClear struct {
	RoomID string
	UserID string // target-user-id: the banned or timed-out user
	At     int64  // unix seconds
}

// parseClearchat parses a CLEARCHAT line. ok is false for anything else, and for
// a whole-channel clear.
func parseClearchat(line string) (ChatClear, bool) {
	var c ChatClear
	l, ok := parseIRCLine(line)
	if !ok || l.command != "CLEARCHAT" {
		return c, false
	}
	if c.UserID = l.tags["target-user-id"]; c.UserID == "" {
		return c, false
	}
	c.RoomID = l.tags["room-id"]
	c.At = tmiSeconds(l.tags["tmi-sent-ts"])
	return c, true
}

// tmiSeconds converts a tmi-sent-ts tag (unix milliseconds) to unix seconds. A
// missing or unparseable tag falls back to now: a tombstone with no timestamp is
// still a tombstone, and the current time is far closer to the truth than the
// epoch, which would put the deletion before every message it could describe.
func tmiSeconds(tag string) int64 {
	if ms, err := strconv.ParseInt(tag, 10, 64); err == nil && ms > 0 {
		return ms / 1000
	}
	return time.Now().Unix()
}
