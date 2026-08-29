# Schema, store methods, and the conformance contract

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: — · Unblocks: 02, 03, 05

## Summary

`store.ChatMessage` / `store.ChatStat`, migration `00006_chat_log` on both backends, and the four
store methods behind them.

Postgres partitions `chat_message` by day on `ts_at`; SQLite gets the same three tables with no
partitioning, exactly as `00005` left the ledger. `chat_stats` and `chat_folded` exist on both so
`store/storetest` is testing one program.

## Decisions

- **`msg_id` carries no UNIQUE constraint.** A partitioned table's unique index must include the
  partition key, so it would degrade to "unique per day". Unlike `ledger_ref` in `00004` nothing
  depends on the guarantee, so there is nothing to rescue into its own table.
- **`deleted_at BIGINT NOT NULL DEFAULT 0`, not nullable.** Follows the `0 = none` convention
  `game_rounds.ends_at` and `ledger_opening.through_ts` already use; no reader handles NULL.
- **`login`/`display` denormalized**, per `wordle_wins`. `users` is economy-only and often empty.
- **The migration seeds today + 14 days** of partitions, because the bot starts writing immediately
  and `chat-partition` does not run until 05:30 the next morning.
- **`chat_message_msg` fans out on lookup.** A CLEARMSG knows the message id but not when it was
  sent, so there is no predicate to prune on. ~90 sub-ms probes a few times per stream; guessing a
  window would trade correctness for nothing.

## Done when

- Both `wantSchemaVersion` constants are 6 and both dialects migrate up and down.
- `TestChatMessageIsPartitionedByRange`, `…DefaultPartitionExists`, `…PremakeWindowExists`,
  `TestEveryChatRowHasTsAtMatchingTs`, `TestChatPartitionBoundsAreUTC`,
  `TestChatLogMigrationIsReversible` pass.
- Five `storetest` cases pass on both backends.
