package main

import (
	"context"
	"time"

	"tts/twitch"
)

// Chat is the subset of Twitch the router uses to reply in chat (an interface so
// tests can substitute a fake). broadcasterID comes from the message's room-id
// tag; parentMsgID (the message id tag) threads the reply to the caller.
type Chat interface {
	Reply(broadcasterID, parentMsgID, text string) error
}

// chatSender adapts a *twitch.Client to the Chat interface.
type chatSender struct {
	client *twitch.Client
}

// NewChatSender wraps a Twitch client as a Chat.
func NewChatSender(c *twitch.Client) *chatSender { return &chatSender{client: c} }

func (s *chatSender) Reply(broadcasterID, parentMsgID, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.client.SendChatMessage(ctx, broadcasterID, text, parentMsgID)
}
