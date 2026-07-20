package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Event notifications: a shoutout (auto on an allow-listed viewer's first message
// each stream, or the mod command !so @user) and — via the events loop — an ad
// reminder. Both surface as a chat line plus a transient bottom-left overlay
// toast pushed as the "notify" event.

// notifyData is the overlay toast payload. kind is "shoutout" (avatar + two
// lines) or "ad" (single line with its own icon).
type notifyData struct {
	Kind   string `json:"kind"`
	Line1  string `json:"line1"`
	Line2  string `json:"line2,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

// notify posts an event to chat (chatText) and the overlay toast. Either sink is
// skipped when not configured.
func (r *Router) notify(roomID, chatText, kind, line1, line2, avatar string) {
	if r.chat != nil && chatText != "" {
		if err := r.chat.Send(roomID, chatText); err != nil {
			r.logger.Printf("notify chat: %v", err)
		}
	}
	if r.overlay != nil {
		r.overlay.Push("notify", notifyData{Kind: kind, Line1: line1, Line2: line2, Avatar: avatar})
	}
}

// maybeShoutout fires an auto-shoutout when m is from an allow-listed login that
// hasn't been shouted yet this stream session (and the stream is live).
func (r *Router) maybeShoutout(m ChatMessage) {
	if len(r.shoutAllow) == 0 || !r.sessionLive.Load() {
		return
	}
	if !r.shoutAllow[m.User] {
		return
	}
	r.shoutMu.Lock()
	if r.shouted == nil {
		r.shouted = make(map[string]bool)
	}
	if r.shouted[m.User] {
		r.shoutMu.Unlock()
		return
	}
	r.shouted[m.User] = true
	r.shoutMu.Unlock()

	r.doShoutout(m.User, displayName(m), m.UserID, m.RoomID)
}

// soCommand handles !so @user (mods/broadcaster only) — a manual shoutout.
func (r *Router) soCommand(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(rest), "@"))
	if login == "" {
		r.reply(m, "usage: !so @user")
		return
	}
	id, display, ok := r.resolveTarget(login)
	if !ok {
		r.reply(m, "no such user @"+login+".")
		return
	}
	r.doShoutout(login, display, id, m.RoomID)
}

// doShoutout composes the shoutout from the target's last-streamed game + avatar
// and emits it to chat + overlay. The game clause/line is dropped when unknown.
func (r *Router) doShoutout(login, display, userID, roomID string) {
	var game, avatar string
	if r.info != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		g, a, err := r.info.ShoutoutInfo(ctx, userID)
		cancel()
		if err != nil {
			r.logger.Printf("shoutout info %s: %v", login, err)
		}
		game, avatar = g, a
	}

	chat := fmt.Sprintf("📢 Go show @%s some love", display)
	line2 := ""
	if game != "" {
		chat += " — they were last streaming " + game
		line2 = "Last streaming " + game
	}
	chat += fmt.Sprintf("! twitch.tv/%s", login)

	r.notify(roomID, chat, "shoutout", "Show @"+display+" some love", line2, avatar)
	r.logger.Printf("shoutout: %s", login)
}

// resetShoutSession clears the per-session shouted set (called when the stream
// goes live).
func (r *Router) resetShoutSession() {
	r.shoutMu.Lock()
	r.shouted = make(map[string]bool)
	r.shoutMu.Unlock()
}
