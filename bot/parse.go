package main

import "strings"

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
}

// parsePrivmsg parses one raw IRCv3 line. ok is false for non-PRIVMSG lines.
//
// Example line:
//
//	@badges=moderator/1;display-name=Bob;emotes=25:5-9;mod=1;subscriber=0 :bob!bob@bob.tmi.twitch.tv PRIVMSG #chan :!tts Kappa hi
func parsePrivmsg(line string) (ChatMessage, bool) {
	var m ChatMessage
	rest := line
	tags := map[string]string{}

	if strings.HasPrefix(rest, "@") {
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			return m, false
		}
		for _, kv := range strings.Split(rest[1:sp], ";") {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				tags[kv[:i]] = kv[i+1:]
			}
		}
		rest = rest[sp+1:]
	}

	// prefix: :nick!user@host
	if !strings.HasPrefix(rest, ":") {
		return m, false
	}
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return m, false
	}
	prefix := rest[1:sp]
	rest = rest[sp+1:]

	// command
	sp = strings.IndexByte(rest, ' ')
	if sp < 0 || rest[:sp] != "PRIVMSG" {
		return m, false
	}
	rest = rest[sp+1:]

	// params: #channel :message
	sp = strings.IndexByte(rest, ' ')
	if sp < 0 {
		return m, false
	}
	channel := strings.TrimPrefix(rest[:sp], "#")
	text := strings.TrimPrefix(rest[sp+1:], ":")

	login := prefix
	if i := strings.IndexByte(prefix, '!'); i >= 0 {
		login = prefix[:i]
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
	m.IsBroadcaster = strings.Contains(tags["badges"], "broadcaster/") || m.User == strings.ToLower(channel)
	if m.IsBroadcaster {
		m.IsMod = true // broadcaster is implicitly a mod
	}
	return m, true
}
