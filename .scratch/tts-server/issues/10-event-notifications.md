# Event notification popups: shoutouts (!so + auto) + ad reminder

Status: done — all 3 stages shipped (2026-07-20)
Type: task
Created: 2026-07-20

PRD: [`../PRD-event-notifications.md`](../PRD-event-notifications.md)

## Summary

Add a transient notification surface — a bottom-left overlay toast (5s, fade) + a chat line — driven
by a new SSE `notify` event. Two producers: **shoutouts** (auto on an allow-listed user's first chat
message, and the mod command `!so @user`) and an **ad reminder** (~60s before a scheduled ad, from
polling the Helix Ad Schedule).

## Decisions (from grilling)

- **Trigger:** shoutout fires on an allow-listed login's first chat message (no EventSub → first
  message is the only reliable signal). Dedup **once per stream session**, reset on offline→live.
- **Ad source:** poll Helix Ad Schedule (`next_ad_at`); needs `channel:read:ads` (re-auth). Warn when
  `0 < until(next) <= lead` (default 60s), deduped per timestamp.
- **`!so`:** mods/broadcaster only; reuses the shoutout emit; hidden from public `!commands`.
- **Toast:** card, bottom-left, slide-in → hold 5s → fade, queued one-at-a-time. `shoutout` = avatar
  + 2 lines; `ad` = 📺 icon + 1 line. Transient: broadcast, **not** cached/replayed.
- **Copy:** shoutout chat `📢 Go show @X some love — they were last streaming <game>! twitch.tv/x`
  (drop game clause if none); overlay `Show @X some love` / `Last streaming <game>`. Ad `📺 Heads
  up! Ads in about a minute — don't go anywhere, back soon ❤️`.

## Architecture

```
Twitch chat ─IRC→ bot.Router.Handle ─(top)→ Shoutouts.Auto(first msg, allow-list, once/session)
!so @user   ─────→ Shoutouts.Manual                         │ Helix: ChannelInfo(game)+GetUserByID(avatar)
Events loop  ─poll→ StreamInfo (live→reset session) + AdSchedule (warn ~60s ahead)
                          │  both emit via Router.notify(roomID, chat, kind, line1, line2, avatar)
                          ▼
        chat.Send  +  overlay.Push("notify", {...})  ──POST /overlay/state (kind=notify, transient)──▶
                          server broadcast (no cache) ──SSE notify──▶ overlay bottom-left toast (5s)
```

## Work breakdown (staged, each shippable)

1. **Transport:** `POST /overlay/state` accepts transient `kind:"notify"` → `broadcast` without
   caching; overlay renders a bottom-left toast card (slide-in/5s/fade), queued. (`server/overlay.go`,
   `server/web/overlay/{index.html,overlay.css,overlay.js}`.)
2. **Shoutouts:** `twitch.GetChannelInfo` + `GetUserByID`/`User.AvatarURL`; `TwitchInfo.ChannelInfo`;
   `bot/shoutout.go` (allow-list, once/session `shouted` set, `Auto`/`Manual`/`ResetSession`);
   `Router.notify` helper; `Auto` at the top of `Router.Handle`; `!so` dispatch + `isBuiltin`;
   `notifications.toml` `[shoutout]` load. (`twitch/api.go`, `bot/info.go`, `bot/shoutout.go`,
   `bot/router.go`, `bot/commands.go`, `bot/config.go`, `bot/main.go`.)
3. **Ad reminder + session:** `twitch.AdSchedule`; `bot/events.go` poll loop (live tracking →
   `ResetSession`; ad warn via pure `adDue`); `channel:read:ads` gate + scope in `cmd/bot-auth`;
   `[ads]` config. (`twitch/api.go`, `bot/events.go`, `bot/main.go`, `cmd/bot-auth/main.go`.)

## Config (`notifications.toml`, opt-in)

```toml
[shoutout]
allow = ["friendlystreamer", "anotherpal"]   # logins auto-shouted on their first message
[ads]
lead    = "60s"   # how far ahead to warn
poll    = "30s"   # ad-schedule poll interval
message = "📺 Heads up! Ads in about a minute — don't go anywhere, back soon ❤️"
```

## Tests

- **Server:** `notify` broadcasts to a live client but is **not** replayed to a fresh connection
  (vs. the cached state kinds); httptest.
- **Bot:** `!so` message with/without game (fake `ChannelInfo`+avatar); auto-trigger once →
  `ResetSession` → again; `adDue(next, now, lead, lastWarned)` window+dedup; config parse.
- **Manual/live:** curl a `notify` toast (shoutout card + ad icon), reload source → no replay; add a
  login and have them chat; `!so @x`; re-auth for `channel:read:ads`, confirm ad warning ~1 min out.

## Progress log

- **Stage 1 (done):** transient `notify` kind on `POST /overlay/state` (broadcast, never
  cached/replayed); bottom-left toast card (slide-in / 5s / fade), queued; shoutout = avatar + 2
  lines, ad = single line; toast title text white. Verified live (shoutout card + ad toast) + a
  reload confirms no replay.
- **Stage 2 (done):** Helix `GetChannelInfo` + avatar (`profile_image_url`, `GetUserByID`);
  `TwitchInfo.ShoutoutInfo`; `Router.notify`; `!so @user` (mods) + allow-list auto-trigger
  (once/session, gated on `sessionLive`) at the top of `Handle`; `notifications.toml` load.
- **Stage 3 (done):** Helix `AdSchedule`; `Events` poll loop (live tracking → `resetShoutSession`
  + `sessionLive`; ad reminder via pure `adDue` + dedup); `channel:read:ads` scope + bot-auth;
  `notifications.toml` sample.

Full `go build/vet/test ./...` green; `go test -race ./bot ./server` clean. Live check of the
overlay toasts done via the SSE path.

Follow-ups (not blocking): the live ad-reminder path needs a re-auth (`mise run bot:auth`) for
`channel:read:ads` and a channel with scheduled ads to exercise end-to-end.

## Out of scope

- EventSub (real join/raid/follow/sub); manual `!ad`; notification sounds.

## References

- Overlay SSE hub + state push: `server/overlay.go` (`broadcast`/`pushState`/`stateKinds`).
- Bot overlay push: `bot/overlay.go` (`OverlayClient`). Helix slice: `bot/info.go` (`TwitchInfo`).
- Target resolution: `bot/admin.go` (`resolveTarget`). Economy poll loop pattern: `bot/economy.go`.
- Auth scopes: `cmd/bot-auth/main.go`; scope gating: `bot/main.go` (`hasEconomyScopes`).
