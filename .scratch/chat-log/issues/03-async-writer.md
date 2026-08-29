# The batching writer, and wiring it into the bot

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: 01, 02 · Unblocks: —

> **Highest-risk issue in the epic.** It puts new work on the single-threaded loop that also answers
> `!tts`. The mitigation is that the loop's only new work is a non-blocking channel send.

## Summary

`bot/chatlog.go`: the `ChatLog` capability interface plus `ChatLogger`, a bounded channel (2048) fed
by the read loop and drained by one writer goroutine batching at 256 rows or 200ms.

## Decisions

- **Drop, never block.** A full buffer discards and counts. "All chat messages" gains a caveat, and
  the caveat is logged rather than hidden.
- **Tombstones share the channel** with messages, and the writer **flushes before applying one** —
  the target may still be in `pending`, where an UPDATE cannot see it.
- **`Wait` is bounded and deferred after the store's `Close`.** Defers run LIFO, so the last batch
  is written while the handle is still open; the bound is there so a wedged database does not also
  mean a bot that will not exit.
- **`-chat-log` defaults true.** Unlike the TOML-configured features this has nothing to configure,
  and every line it is off for is unrecoverable.

## Done when

Eight writer cases pass, including drop-on-full asserted with no writer running at all (the limit
case of a dead database), and `go test -race` is clean.
