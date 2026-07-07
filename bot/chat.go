package main

import (
	"context"
	"time"

	"tts/twitch"
)

// Chat is the subset of Twitch the router uses to post in chat (an interface so
// tests can substitute a fake). broadcasterID comes from the message's room-id
// tag; Reply threads to the caller via parentMsgID (the message id tag), Send
// posts a plain channel message (used by timers).
type Chat interface {
	Reply(broadcasterID, parentMsgID, text string) error
	Send(broadcasterID, text string) error
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

func (s *chatSender) Send(broadcasterID, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.client.SendChatMessage(ctx, broadcasterID, text, "")
}
