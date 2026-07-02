package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"
)

const twitchIRC = "irc.chat.twitch.tv:6697"

// IRCClient maintains an anonymous read-only connection to Twitch chat and
// dispatches each PRIVMSG to handle. It reconnects with backoff until ctx ends.
type IRCClient struct {
	channel string
	logger  *log.Logger
	rnd     *rand.Rand
	handle  func(ChatMessage)
}

func (c *IRCClient) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err := c.serve(ctx); err != nil && ctx.Err() == nil {
			c.logger.Printf("irc: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		c.logger.Printf("reconnecting in %s", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (c *IRCClient) serve(ctx context.Context) error {
	dialer := &tls.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", twitchIRC)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Closing the conn on ctx cancel unblocks the read loop.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	nick := fmt.Sprintf("justinfan%d", 10000+c.rnd.Intn(89999))
	send := func(s string) error {
		_, e := conn.Write([]byte(s + "\r\n"))
		return e
	}
	if err := send("CAP REQ :twitch.tv/tags twitch.tv/commands"); err != nil {
		return err
	}
	if err := send("NICK " + nick); err != nil {
		return err
	}
	if err := send("JOIN #" + c.channel); err != nil {
		return err
	}
	c.logger.Printf("connected as %s, joined #%s", nick, c.channel)

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.HasPrefix(line, "PING") {
			_ = send("PONG :tmi.twitch.tv")
			continue
		}
		if m, ok := parsePrivmsg(line); ok {
			c.handle(m)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("connection closed")
}
