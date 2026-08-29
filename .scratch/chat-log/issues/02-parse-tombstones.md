# Parse CLEARMSG/CLEARCHAT and dispatch them in order

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: 01 · Unblocks: 03

## Summary

`parsePrivmsg` only handled PRIVMSG, but the `twitch.tv/commands` capability was already being
requested — so both tombstone commands were already arriving and being dropped. Extract
`parseIRCLine` for the shared tag/prefix/command split, then add `parseClearmsg` and
`parseClearchat`.

## Decisions

- **A whole-channel CLEARCHAT is ignored.** No `target-user-id` means a mod ran `/clear`, which
  tidies the display and says nothing about the messages. Tombstoning the channel's history on a
  routine action would erase the distinction the log exists to record.
- **`ban-duration` is not carried.** A tombstone records that a CLEARCHAT removed the line; nothing
  downstream distinguishes a timeout from a ban, and an unread field is a question about why it is
  there.
- **`room-id` may be empty on CLEARMSG** and is left empty rather than guessed at; the caller
  substitutes the room it has been tracking.
- **A missing `tmi-sent-ts` falls back to now**, not the epoch, which would place the deletion
  before every message it could describe.
- **All three dispatch from the same read loop**, in arrival order. That ordering is what the
  writer depends on.

## Done when

Six parser cases pass, including that each parser rejects the other two commands.
