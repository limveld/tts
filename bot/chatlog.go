package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"tts/store"
)

// The chat log. Every PRIVMSG the bot sees is persisted, along with the
// tombstones Twitch's CLEARMSG and CLEARCHAT put on them.
//
// The whole design turns on one guarantee: logging must never stall the IRC read
// loop. That loop is single-threaded (see IRCClient.serve) and is also what
// answers !tts, so a synchronous INSERT would mean a wedged database makes the
// bot go deaf. Instead the read loop does a non-blocking send into a buffered
// channel and one writer goroutine owns every database call.
//
// The honest cost of that choice is that a full buffer drops lines rather than
// waiting. "All chat messages" therefore means "all chat messages unless the
// database is unavailable for long enough to fill the buffer" — which is why the
// drop count is logged rather than swallowed.
//
// See docs/adr/0003-chat-log.md.

// ChatLog is the persistence slice the chat log uses (an interface so tests can
// substitute a fake). *sqlite.Store and *postgres.Store satisfy it.
type ChatLog interface {
	LogMessages(msgs []store.ChatMessage) error
	MarkDeleted(roomID, msgID string, at int64) (found bool, err error)
	MarkUserCleared(roomID, userID string, since, at int64) (n int64, err error)
	UserMessages(userID string, limit int) ([]store.ChatMessage, error)
}

const (
	// chatBuffer is how many events can be in flight before sends start dropping.
	// At this channel's real load — a couple of hundred messages a day — it is
	// never more than a few deep. It is sized for the two cases that are not the
	// average: a raid burst, and a database that has stopped answering. At raid
	// rates this absorbs something like twenty seconds of chat before the first
	// line is lost.
	chatBuffer = 2048

	// chatFlushRows and chatFlushInterval are the two things that end a batch,
	// whichever comes first. The interval is what bounds how long a line can sit
	// unwritten on a quiet channel; the row count is what stops a raid from
	// building an unboundedly large INSERT.
	chatFlushRows     = 256
	chatFlushInterval = 200 * time.Millisecond

	// clearChatWindow bounds how far back a CLEARCHAT tombstones. Twitch clears
	// the visible buffer, not the channel's history, so tombstoning everything a
	// person ever said would claim more than actually happened.
	clearChatWindow = 24 * time.Hour

	// dropReportInterval rate-limits the "buffer full" log line. Without it a
	// sustained outage would print five times a second and bury its own cause.
	dropReportInterval = 30 * time.Second
)

type chatEventKind uint8

const (
	eventMessage chatEventKind = iota
	eventDelete
	eventClear
)

// chatEvent is one unit of work for the writer: a line to append, or a tombstone
// to apply. All three kinds travel the same channel so the writer applies them
// in the order the read loop saw them. That ordering is not decorative — a
// CLEARMSG that overtook the message it targets would match no row and the
// deletion would be lost.
type chatEvent struct {
	kind chatEventKind
	msg  store.ChatMessage
	del  ChatDelete
	clr  ChatClear
}

// ChatLogger buffers chat events and writes them in batches. Construct with
// NewChatLogger and run Run on its own goroutine.
type ChatLogger struct {
	store  ChatLog
	room   func() string // fallback room id for tombstones that arrive without one
	logger *log.Logger
	events chan chatEvent
	done   chan struct{}
	now    func() time.Time

	dropped atomic.Int64 // written by the read loop, read by the writer

	// Writer-goroutine state. Not guarded, because only Run touches it.
	reported     int64
	lastReported time.Time
}

// NewChatLogger builds a logger writing to st. room supplies the broadcaster id
// for tombstones that arrive without a room-id tag, which Twitch does not
// reliably set on CLEARMSG.
func NewChatLogger(st ChatLog, room func() string, logger *log.Logger) *ChatLogger {
	return &ChatLogger{
		store:  st,
		room:   room,
		logger: logger,
		events: make(chan chatEvent, chatBuffer),
		done:   make(chan struct{}),
		now:    time.Now,
	}
}

// Message enqueues one chat line. Called from the IRC read loop, so it never
// blocks: a full buffer drops the line and counts it.
func (c *ChatLogger) Message(m ChatMessage) {
	c.send(chatEvent{kind: eventMessage, msg: toStoreMessage(m, c.now().Unix())})
}

// Delete enqueues a CLEARMSG tombstone.
func (c *ChatLogger) Delete(d ChatDelete) {
	if d.RoomID == "" {
		d.RoomID = c.room()
	}
	if d.RoomID == "" {
		// Nothing to scope the tombstone to, and an unscoped UPDATE would reach
		// across every room the log has ever held. Dropping it is the safe half of
		// a bad choice, and it can only happen before the first PRIVMSG arrives.
		c.logger.Printf("chatlog: clearmsg %s dropped — no room id yet", d.MsgID)
		return
	}
	c.send(chatEvent{kind: eventDelete, del: d})
}

// Clear enqueues a CLEARCHAT tombstone.
func (c *ChatLogger) Clear(cl ChatClear) {
	if cl.RoomID == "" {
		cl.RoomID = c.room()
	}
	if cl.RoomID == "" {
		c.logger.Printf("chatlog: clearchat %s dropped — no room id yet", cl.UserID)
		return
	}
	c.send(chatEvent{kind: eventClear, clr: cl})
}

func (c *ChatLogger) send(e chatEvent) {
	select {
	case c.events <- e:
	default:
		c.dropped.Add(1)
	}
}

// Dropped reports how many events have been discarded for want of buffer space.
func (c *ChatLogger) Dropped() int64 { return c.dropped.Load() }

// Run owns every database call the chat log makes. It returns once ctx is done
// and everything still buffered has been written.
func (c *ChatLogger) Run(ctx context.Context) {
	defer close(c.done)

	ticker := time.NewTicker(chatFlushInterval)
	defer ticker.Stop()

	pending := make([]store.ChatMessage, 0, chatFlushRows)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		if err := c.store.LogMessages(pending); err != nil {
			// The batch is gone either way; saying how many went with it is the
			// difference between a known gap and a mystery.
			c.logger.Printf("chatlog: lost %d messages: %v", len(pending), err)
		}
		pending = pending[:0]
	}

	apply := func(e chatEvent) {
		switch e.kind {
		case eventMessage:
			pending = append(pending, e.msg)
			if len(pending) >= chatFlushRows {
				flush()
			}
		case eventDelete:
			// The target may still be sitting in pending, where an UPDATE cannot
			// see it. Flushing first is what makes the read loop's ordering
			// survive the trip through the buffer.
			flush()
			found, err := c.store.MarkDeleted(e.del.RoomID, e.del.MsgID, e.del.At)
			if err != nil {
				c.logger.Printf("chatlog: clearmsg %s: %v", e.del.MsgID, err)
			} else if !found {
				// Normal: the message may predate logging, or have been dropped
				// when the buffer was full.
				c.logger.Printf("chatlog: clearmsg %s matched no logged message", e.del.MsgID)
			}
		case eventClear:
			flush()
			since := e.clr.At - int64(clearChatWindow/time.Second)
			n, err := c.store.MarkUserCleared(e.clr.RoomID, e.clr.UserID, since, e.clr.At)
			if err != nil {
				c.logger.Printf("chatlog: clearchat %s: %v", e.clr.UserID, err)
				return
			}
			c.logger.Printf("chatlog: clearchat %s tombstoned %d messages", e.clr.UserID, n)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Drain what is already buffered rather than abandoning it: these are
			// lines the read loop accepted, and shutdown is not a reason to lose
			// them. Nothing new can arrive that matters — the read loop is
			// unwinding on the same cancellation.
			for {
				select {
				case e := <-c.events:
					apply(e)
				default:
					flush()
					c.reportDropped(true)
					return
				}
			}
		case e := <-c.events:
			apply(e)
		case <-ticker.C:
			flush()
			c.reportDropped(false)
		}
	}
}

// Wait blocks until Run has drained and flushed, or until timeout. main defers
// it ahead of the store's Close so the final batch is written while the handle
// it needs is still open — and bounds it, because a wedged database must not
// also mean a bot that will not exit.
func (c *ChatLogger) Wait(timeout time.Duration) {
	select {
	case <-c.done:
	case <-time.After(timeout):
		c.logger.Printf("chatlog: shutdown flush timed out after %s", timeout)
	}
}

// reportDropped logs the drop count when it has moved, at most once per
// dropReportInterval. force ignores the interval, for the final report at
// shutdown. Dropping is the deliberate consequence of never blocking the read
// loop; a silent drop would make "all chat messages" a claim nobody can check.
func (c *ChatLogger) reportDropped(force bool) {
	n := c.dropped.Load()
	if n == c.reported {
		return
	}
	now := c.now()
	if !force && now.Sub(c.lastReported) < dropReportInterval {
		return
	}
	c.logger.Printf("chatlog: dropped %d events (buffer full), %d since the last report",
		n, n-c.reported)
	c.reported, c.lastReported = n, now
}

// toStoreMessage converts the bot's parsed line into the store's row. The two
// types are deliberately separate — see store.ChatMessage — and this is the only
// place that knows both.
//
// IsMod carries the parser's convenience that a broadcaster is implicitly a mod
// (parse.go). That is the router's view rather than Twitch's, but IsBroadcaster
// is stored alongside it, so nothing is lost: "mod badge" is is_mod AND NOT
// is_broadcaster.
func toStoreMessage(m ChatMessage, ts int64) store.ChatMessage {
	return store.ChatMessage{
		TS:            ts,
		RoomID:        m.RoomID,
		MsgID:         m.ID,
		UserID:        m.UserID,
		Login:         m.User,
		Display:       m.Display,
		Text:          m.Text,
		Emotes:        m.Emotes,
		IsMod:         m.IsMod,
		IsSub:         m.IsSub,
		IsVIP:         m.IsVIP,
		IsBroadcaster: m.IsBroadcaster,
	}
}
