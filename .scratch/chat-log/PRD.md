# PRD: Persist the chat log

Created: 2026-08-29
Status: shipped (2026-08-29)

ADR: [`docs/adr/0003-chat-log.md`](../../docs/adr/0003-chat-log.md)

## Problem

The bot sees every line in the channel and keeps almost none of them. `IRCClient.serve` reads each
PRIVMSG, `parsePrivmsg` builds a fully populated `ChatMessage`, and `Router.Handle` throws away
anything not starting with `!`. Every non-command line the channel has ever produced was parsed and
discarded.

## Goals

1. **Archive** — an append-only record of the channel, queryable from `psql`.
2. **Moderation** — answer "what did they say, and was it deleted" for a given user.
3. **Analytics** — per-user activity totals that survive retention.
4. **Substrate** — history for features that cannot be built without it (recap, highlights,
   `!quote`), none of which can be backfilled later.

## Non-goal, stated because it was proposed

Exercising `gopartman` at higher volume than the ledger. **The numbers do not support it.** The
ledger runs 647 rows/day, 98.6% of them `accrual`; its human-driven rows are ~8.5/day, which puts
chat somewhere near 60–250/day. Chat is the *smaller* table. What it genuinely provides is a second
independently-registered parent with a different fold, horizon and gate — a real but smaller reason,
recorded as the smaller one in the ADR.

## Constraints

- **The read loop must never stall.** It is single-threaded and also answers `!tts`.
- **Both backends.** `store/storetest` is the contract; every behavioral table exists on both.
- **`cmd/pg-partition` is money-critical.** Any refactor of it must leave its tests passing
  unchanged.
- **No new dependencies.** ADR-0001's near-stdlib budget stays at seven.

## Decisions

| Decision | Choice |
|---|---|
| Scope | PRIVMSG + CLEARMSG/CLEARCHAT tombstones. USERNOTICE deferred. |
| Write path | Bounded channel → one batching writer. Drops rather than blocks. |
| Retention | Daily partitions, 90 days, fold counters into `chat_stats` first. |
| Analytics | Fold-only counters (`ledger_opening` pattern). No runtime reads. |
| Row shape | Typed columns, `login`/`display` denormalized, raw text. |
| Tooling | Shared `internal/partition`; `pg-partition` + `chat-partition` on top. |
| Tombstones | Keep text, set `deleted_at`/`deleted_by`. `-purge-user` for erasure. |
| Drop gate | Reports drift; does not block. Nothing here is money. |

## Issues

1. [`01-schema-and-store.md`](issues/01-schema-and-store.md) — migration 00006, store methods,
   conformance
2. [`02-parse-tombstones.md`](issues/02-parse-tombstones.md) — CLEARMSG/CLEARCHAT parsing and
   dispatch
3. [`03-async-writer.md`](issues/03-async-writer.md) — the batching writer and its wiring
4. [`04-shared-partition-core.md`](issues/04-shared-partition-core.md) — extract
   `internal/partition`
5. [`05-chat-partition.md`](issues/05-chat-partition.md) — the maintenance job and `-purge-user`
6. [`06-migrate-deploy-docs.md`](issues/06-migrate-deploy-docs.md) — `store-migrate`, launchd, ADR
