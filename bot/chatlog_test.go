package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"tts/store"
)

// fakeChatLog records what the writer asked for, in order. The order is the
// point of most of these cases: a tombstone applied before the batch holding its
// target would match nothing.
type fakeChatLog struct {
	mu    sync.Mutex
	calls []string
	batch []int // rows in each LogMessages call, in order
	rows  []store.ChatMessage

	block chan struct{} // when non-nil, LogMessages waits on it
}

func (f *fakeChatLog) LogMessages(msgs []store.ChatMessage) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("log:%d", len(msgs)))
	f.batch = append(f.batch, len(msgs))
	f.rows = append(f.rows, msgs...)
	return nil
}

func (f *fakeChatLog) MarkDeleted(roomID, msgID string, at int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete:"+roomID+"/"+msgID)
	return true, nil
}

func (f *fakeChatLog) MarkUserCleared(roomID, userID string, since, at int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("clear:%s/%s/since=%d", roomID, userID, since))
	return 1, nil
}

func (f *fakeChatLog) UserMessages(string, int) ([]store.ChatMessage, error) { return nil, nil }

func (f *fakeChatLog) snapshot() ([]string, []int, []store.ChatMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([]int(nil), f.batch...), append([]store.ChatMessage(nil), f.rows...)
}

// waitFor polls until cond holds or the deadline passes. The writer runs on its
// own goroutine and flushes on a timer, so every assertion about what it did is
// eventually-true rather than immediately-true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestChatLogger(t *testing.T, f *fakeChatLog) (*ChatLogger, context.CancelFunc) {
	t.Helper()
	c := NewChatLogger(f, func() string { return "tracked-room" }, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() { cancel(); c.Wait(2 * time.Second) })
	return c, cancel
}

func testLine(id string) ChatMessage {
	return ChatMessage{
		ID: id, UserID: "u1", User: "bob", Display: "Bob",
		RoomID: "room1", Text: "hello",
	}
}

func TestChatLoggerFlushesOnRowCount(t *testing.T) {
	f := &fakeChatLog{}
	c, _ := newTestChatLogger(t, f)

	// More than one batch worth. A writer that only flushed on its timer would
	// send all 300 in one call; flushing on the row count caps the first at
	// chatFlushRows, which is what stops a raid building an unbounded INSERT.
	for i := range 300 {
		c.Message(testLine(fmt.Sprintf("m%d", i)))
	}

	waitFor(t, "300 rows written", func() bool {
		_, _, rows := f.snapshot()
		return len(rows) == 300
	})
	_, batches, _ := f.snapshot()
	if batches[0] != chatFlushRows {
		t.Errorf("first batch=%d want %d — it flushed on the timer, not the row count", batches[0], chatFlushRows)
	}
}

func TestChatLoggerFlushesOnTimer(t *testing.T) {
	f := &fakeChatLog{}
	c, _ := newTestChatLogger(t, f)

	// A single line, far below the row threshold. Nothing but the timer can get
	// it out, and on a channel this quiet the timer is the only thing that ever
	// does.
	c.Message(testLine("m1"))

	waitFor(t, "the timer to flush one line", func() bool {
		_, _, rows := f.snapshot()
		return len(rows) == 1
	})
	_, _, rows := f.snapshot()
	if rows[0].MsgID != "m1" || rows[0].Login != "bob" || rows[0].RoomID != "room1" {
		t.Errorf("row=%+v did not carry the message through", rows[0])
	}
	if rows[0].TS == 0 {
		t.Error("TS=0: the writer did not stamp the row")
	}
}

func TestChatLoggerFlushesOnCancel(t *testing.T) {
	f := &fakeChatLog{}
	c := NewChatLogger(f, func() string { return "tracked-room" }, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)

	for i := range 10 {
		c.Message(testLine(fmt.Sprintf("m%d", i)))
	}
	// Cancel immediately: these are lines the read loop already accepted, and
	// shutting down is not a reason to lose them.
	cancel()
	c.Wait(2 * time.Second)

	_, _, rows := f.snapshot()
	if len(rows) != 10 {
		t.Errorf("wrote %d rows after cancel, want 10 — the shutdown drain lost some", len(rows))
	}
}

// The guarantee the whole design exists for: a send from the IRC read loop never
// blocks, whatever the writer is doing. Tested with no writer at all, which is
// the limit case of a database that has stopped answering.
func TestChatLoggerDropsWhenBufferFull(t *testing.T) {
	f := &fakeChatLog{}
	c := NewChatLogger(f, func() string { return "tracked-room" }, log.New(io.Discard, "", 0))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range chatBuffer + 10 {
			c.Message(testLine(fmt.Sprintf("m%d", i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Message blocked with a full buffer — the read loop would have stalled")
	}

	if got := c.Dropped(); got != 10 {
		t.Errorf("dropped=%d want 10 (buffer is %d, sent %d)", got, chatBuffer, chatBuffer+10)
	}
}

// A CLEARMSG must not overtake the message it targets. The writer holds unsent
// lines in memory, where an UPDATE cannot see them, so it has to flush before
// applying a tombstone.
func TestChatLoggerFlushesBeforeTombstone(t *testing.T) {
	f := &fakeChatLog{}
	c, _ := newTestChatLogger(t, f)

	c.Message(testLine("m1"))
	c.Delete(ChatDelete{RoomID: "room1", MsgID: "m1", At: 500})

	waitFor(t, "the tombstone to be applied", func() bool {
		calls, _, _ := f.snapshot()
		return len(calls) == 2
	})
	calls, _, _ := f.snapshot()
	if calls[0] != "log:1" || calls[1] != "delete:room1/m1" {
		t.Errorf("calls=%v want the batch written before the tombstone", calls)
	}
}

func TestChatLoggerClearUsesBoundedWindow(t *testing.T) {
	f := &fakeChatLog{}
	c, _ := newTestChatLogger(t, f)

	c.Clear(ChatClear{RoomID: "room1", UserID: "u1", At: 100_000})

	waitFor(t, "the clearchat to be applied", func() bool {
		calls, _, _ := f.snapshot()
		return len(calls) == 1
	})
	calls, _, _ := f.snapshot()
	// A ban clears the visible buffer, not the channel's whole history, so the
	// since bound is what keeps the tombstone honest about what Twitch did.
	want := fmt.Sprintf("clear:room1/u1/since=%d", int64(100_000)-int64(clearChatWindow/time.Second))
	if calls[0] != want {
		t.Errorf("call=%q want %q", calls[0], want)
	}
}

// Twitch does not reliably set room-id on CLEARMSG, so the logger substitutes
// the room it has been tracking from PRIVMSG tags.
func TestChatLoggerFallsBackToTrackedRoom(t *testing.T) {
	f := &fakeChatLog{}
	c, _ := newTestChatLogger(t, f)

	c.Delete(ChatDelete{MsgID: "m1", At: 500})

	waitFor(t, "the tombstone to be applied", func() bool {
		calls, _, _ := f.snapshot()
		return len(calls) == 1
	})
	calls, _, _ := f.snapshot()
	if calls[0] != "delete:tracked-room/m1" {
		t.Errorf("call=%q want the tracked room substituted", calls[0])
	}
}

// Before the first PRIVMSG there is no room to fall back to, and an unscoped
// UPDATE would reach across every room the log has ever held. Dropping the
// tombstone is the safe half of a bad choice.
func TestChatLoggerDropsRoomlessTombstone(t *testing.T) {
	f := &fakeChatLog{}
	c := NewChatLogger(f, func() string { return "" }, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer func() { cancel(); c.Wait(2 * time.Second) }()

	c.Delete(ChatDelete{MsgID: "m1", At: 500})
	c.Clear(ChatClear{UserID: "u1", At: 500})

	time.Sleep(3 * chatFlushInterval)
	if calls, _, _ := f.snapshot(); len(calls) != 0 {
		t.Errorf("calls=%v want none: neither tombstone had a room to scope to", calls)
	}
}
