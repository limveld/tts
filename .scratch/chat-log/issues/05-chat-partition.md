# cmd/chat-partition: fold, drop, and purge

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: 01, 04 · Unblocks: 06

## Summary

The chat log's maintenance job: provision, fold per-user counts into `chat_stats` in the same
transaction as the DETACH, report on the totals, drop. Plus `-purge-user` for erasure requests.

## Decisions

- **The drop is not gated.** `pg-partition` blocks on a balance reconcile because the ledger is
  money. A chat count is not, and `-purge-user` legitimately moves `chat_folded` and `chat_stats`
  apart — a gate would turn every erasure request into a stuck nightly job. The comparison is
  printed instead.
- **90-day horizon**, against the ledger's 365. No moderation question reaches back past a quarter.
- **`-purge-user` deletes both halves in one transaction.** Removing the messages but leaving the
  stats row would leave a count of lines nobody can read, which is the opposite of the request.
- **Names in `chat_stats` come from `array_agg(... ORDER BY ts DESC)[1]`**, not a `GROUP BY`, so a
  mid-day rename does not split one person into two rows.
- **`LEAST(NULLIF(first_ts, 0), …)`** so the column default loses to a real timestamp instead of
  winning as the smallest number available.

## Done when

Eight cases pass, including fold idempotency, the crash-after-commit recovery, `-dry-run` proven
read-only, the purge reaching folded history, and `-backfill` rescuing the default partition with
every column intact.
