# PRD: Event notification popups (shoutouts + ad reminder)

Status: ready-for-agent
Type: prd
Created: 2026-07-20

Tracked in detail at [`issues/10-event-notifications.md`](issues/10-event-notifications.md).

## Problem Statement

The overlay and bot have no "event" surface. Two moments the broadcaster wants to celebrate/announce
go unmarked today:

- **A notable person shows up** — a friend/fellow-streamer on an allow-list joins chat, and there's
  no automatic "give them a follow" shoutout. There's also no reusable manual shoutout command.
- **An ad break is about to run** — viewers get no heads-up, so the break feels abrupt.

Both should be visible in chat **and** as an on-stream popup.

## Solution

A transient **notification** surface: a new bottom-left overlay toast (5s, fade) plus a matching chat
line, driven by a new **transient SSE `notify` event** (broadcast but — unlike gamble/depth/wordle —
never cached/replayed, so an OBS reload can't resurrect a stale toast).

Two producers on the bot:

1. **Shoutouts** — when an allow-listed login sends its **first chat message of the stream**, the bot
   fetches their last-streamed game + avatar from Helix and emits a shoutout (once per stream
   session). The same output is a mod command **`!so @user`**.
2. **Ad reminder** — a poll loop reads the Helix **Ad Schedule** and, ~60s before the next scheduled
   ad, emits a heads-up.

The bot is IRC-read + Helix-poll (no EventSub), so "entered" is observed as the first chat message,
and the ad schedule is polled rather than pushed.

## User Stories

1. As a broadcaster, I want an allow-listed friend to get an automatic follow-shoutout when they first
   chat, so I never forget to plug them.
2. As a mod, I want `!so @user` to fire that same shoutout on demand, so it's reusable any time.
3. As a viewer, I want the shoutout as an on-stream card (their avatar + "Show @X some love / Last
   streaming <game>"), so it's noticeable, not just a chat line.
4. As a broadcaster, I want a shoutout to fire at most once per person per stream, so re-entry doesn't
   spam chat/overlay.
5. As a viewer, I want a ~1-minute warning before an ad break in chat and on-screen, so the break
   isn't abrupt.
6. As a broadcaster, I want notifications to never linger or replay on an OBS source reload, so the
   overlay stays clean.

## Implementation Decisions

- **Transport:** a transient `notify` SSE event via the existing `POST /overlay/state` (kind
  `notify`), broadcast without caching. Payload `{kind, line1, line2?, avatar?}`.
- **Toast:** bottom-left card, slides in from the left, holds 5s, fades out; queued one-at-a-time.
  `shoutout` shows the Twitch avatar + two lines; `ad` shows a 📺 icon + one line.
- **Shoutout trigger:** first chat message from an allow-listed login (only reliable signal without
  EventSub); dedup **once per stream session**, reset on offline→live.
- **Shoutout data:** Helix Get Channel Information (`game_name`) + Get Users (`profile_image_url`).
- **`!so`:** mods/broadcaster only; reuses the same emit path; hidden from the public `!commands`.
- **Ad reminder:** poll Helix Ad Schedule (`next_ad_at`); warn when `0 < until(next) <= lead`
  (default 60s), deduped per scheduled timestamp. Needs `channel:read:ads` (re-auth).
- **Copy:** shoutout chat `📢 Go show @X some love — they were last streaming <game>! twitch.tv/x`
  (game clause dropped if never streamed); overlay `Show @X some love` / `Last streaming <game>`.
  Ad `📺 Heads up! Ads in about a minute — don't go anywhere, back soon ❤️`.
- **Config:** opt-in `notifications.toml` (`[shoutout] allow`, `[ads] lead/poll/message`).

## Testing Decisions

- **Server:** `notify` broadcasts to a connected client but is **not** replayed to a
  freshly-connected one (contrast with the cached state kinds).
- **Bot:** `!so` message with and without a game (fake `ChannelInfo`); auto-trigger fires once then
  again after `ResetSession`; `adDue` window/dedup logic; config parsing.
- **Manual/live:** curl a `notify` and watch the toast; add a login + have them chat; `!so`; re-auth
  and confirm the ad warning ~1 min before a scheduled ad.

## Out of Scope

- EventSub (real join/raid/follow/sub events) — separate, larger effort.
- Non-scheduled/manual ads (`!ad`) — polling the schedule only.
- Sound effects for notifications; per-notification custom styling beyond the two kinds.

## Further Notes

- Reuses the overlay SSE hub (`server/overlay.go`), the `OverlayClient` push (`bot/overlay.go`), the
  `TwitchInfo` Helix slice (`bot/info.go`), and `resolveTarget` (`bot/admin.go`).
- Sequencing: (1) `notify` transport + toast, (2) shoutouts + `!so`, (3) ad reminder + session
  tracking + `channel:read:ads`.
