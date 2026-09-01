package main

// The maze's outbound chat, moved off the game clock.
//
// runMaze drives cycles from a time.Ticker and used to call chat.Send inline.
// Every send is an HTTPS POST to Helix with a ten-second timeout — longer than a
// cycle — and round end adds two store writes and a send per finisher. Go's
// time.Ticker buffers one tick, so a single slow call meant the next tick was
// already waiting and fired the instant the last one returned: two cycles
// resolving back to back, and a sprite that moved twice with nobody touching it.
//
// This is the same shape bot/overlay.go's OverlayClient already uses for the same
// reason: a buffered channel, one worker, and an enqueue that drops rather than
// blocks. One worker rather than several because the game narrates a sequence —
// "@bob took the last key" then "@bob is OUT" reads wrong in the other order, so
// dropping a line is better than reordering one.
//
// Only the maze uses it. The other games send once or twice from a one-shot
// timer, where a slow call delays a message and nothing else; the maze is the
// only one where it corrupts the game.

// mazeChatQueue is the per-Router send buffer. Depth is generous: a whole round
// emits well under this, so a full queue means Helix is wedged rather than that
// the game is chatty.
const mazeChatQueue = 64

type mazeChatMsg struct {
	roomID string
	text   string
}

// startMazeChat brings up the sender. Its lifetime is the Router's, not a round's,
// so there is no goroutine to leak when a round ends and halt() stays purely about
// the ticker.
func (r *Router) startMazeChat() {
	if r.mazeChat != nil {
		return
	}
	r.mazeChat = make(chan mazeChatMsg, mazeChatQueue)
	go func() {
		for msg := range r.mazeChat {
			if r.chat == nil {
				continue
			}
			if err := r.chat.Send(msg.roomID, msg.text); err != nil {
				r.logger.Printf("maze chat: %v", err)
			}
		}
	}()
}

// sendMaze queues one line. It never blocks the caller, which is the whole point:
// this is called from the cycle, and the cycle must not wait on the network.
//
// A full queue drops the line with a log rather than stalling the game. That is
// the right trade for narration — a missing callout is a worse-looking round, a
// late cycle is a broken one. It deliberately does not apply to anything that
// moves marks: payouts stay synchronous in awardMazeFinishers, because a dropped
// payout is not cosmetic.
func (r *Router) sendMaze(roomID, text string) {
	if r.mazeChat == nil { // not started (tests that never call startMazeChat)
		if r.chat != nil {
			if err := r.chat.Send(roomID, text); err != nil {
				r.logger.Printf("maze chat: %v", err)
			}
		}
		return
	}
	select {
	case r.mazeChat <- mazeChatMsg{roomID, text}:
	default:
		r.logger.Printf("maze chat: queue full, dropped %q", text)
	}
}
