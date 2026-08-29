# Extract internal/partition from cmd/pg-partition

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: — · Unblocks: 05

> **Touches a money-critical tool for a non-money feature's benefit.** The whole safety argument is
> that `cmd/pg-partition`'s seven tests pass unchanged afterwards.

## Summary

Move the table-agnostic machinery into `internal/partition`: gopartman registration and `HookSkip`,
`ListChildren` and bounds parsing, `QuoteQualified`, `Lock`, the claim→DETACH→aggregate→commit
skeleton, `DropDetached`, `Backfill`, `Report`.

## Decisions

- **The folds do not move.** The ledger's `SUM(delta) → ledger_opening` under a balance reconcile
  and chat's `COUNT → chat_stats` under no gate are the parts that genuinely differ. `Fold` takes
  a `Claim` and an `Aggregate` callback.
- **Duplication was rejected on this repo's record.** ADR-0002 lists four things the ledger's
  version found the hard way — UTC bounds, the non-read-only `-dry-run`, the advisory lock having
  to be gopartman's own, and the backfill ordering. A second copy re-opens all four.
- **`Parent.Cols` is how the backfill moves rows**, so it has to stay in step with the migration. A
  column missing there is a column silently dropped from every rescued row.

## What the work found

Two races, both from the same cause: partman's registry and schema are **global**, so a second
command turns things that were single-process into concurrent ones. Neither shows up in production —
the agents are 15 minutes apart — and both were found by `go test ./...` running the two suites in
parallel.

1. **Concurrent partman migrations** fail one process with `tuple concurrently updated` (XX000).
   `applyPartmanMigrations` now takes a global advisory lock of our own, since gopartman's is
   per-parent and what needs protecting is the schema the parents share.
2. **`Maintain` maintains every registered parent**, not just the one it was asked about — so each
   tool's tick briefly holds the per-parent lock on the *other* tool's table. `Lock` treated a
   single refusal as "another pass is running" and would have silently skipped a night's retention
   whenever a sibling was mid-tick. It now retries for 15s: long enough to outlast a tick, far too
   short to outlast a fold.

Worth naming the general shape: **adding a second parent turned single-process assumptions into
races, and nothing about "register another table" said it would.**

## Done when

`go test ./cmd/pg-partition/` passes with only mechanical identifier renames in the test file, and
`go test -race ./cmd/...` is stable across repeated runs.
