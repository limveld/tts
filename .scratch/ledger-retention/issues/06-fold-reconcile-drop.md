# The fold, the reconcile gate, and the drop

Status: done (2026-08-11)
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 05 · Unblocks: 07

> The first issue in the epic that destroys data. Everything before it only ever added.

## Summary

Fold expired partitions into `ledger_opening` and drop them — but only after proving, for every
user, that `accounts.balance == ledger_opening + SUM(ledger)`.

## Decisions

- **Detach and fold in one transaction.** PostgreSQL has transactional DDL, so this costs nothing
  and keeps the reconcile invariant true at every instant rather than only between runs. The
  alternative — gopartman's `HookDetach` followed by a post-pass — leaves a window where the
  invariant is false, and "the error is in the safe direction" is an argument you have to re-derive
  every time someone reads the code.
- **Detach first inside that transaction**, taking `ACCESS EXCLUSIVE` on the parent up front so the
  window where readers block is as short as possible. On a ~640-row child at 05:15 with no stream
  running it is milliseconds. `DETACH CONCURRENTLY` cannot run inside a transaction block, which is
  exactly why it is not used.
- **`ledger_folded`'s primary key is the idempotency**, deliberately not `partman.partitions.status`.
  Coupling a money invariant to another library's bookkeeping means a library upgrade can change
  what the money means.
- **The drop is a separate transaction, gated on reconcile.** History is only destroyed once it has
  proved it agrees with the balances that outlive it. A dirty reconcile is a hard error and a
  non-zero exit — launchd will log it and the next run retries.
- **Orphan reconciliation is required, not optional.** A crash between the fold's `COMMIT` and its
  `DROP` leaves a detached child that is invisible to `SELECT … FROM ledger` but *is* in `pg_dump`.
  Left alone they quietly grow every backup.
- **Hold gopartman's own advisory-lock key** — `pg_try_advisory_lock(hashtext(schema),
  hashtext(table))` — across the whole pass, so `Maintain` and the fold can never overlap. Matching
  its key rather than inventing one is what makes that true.

## Work breakdown

**`cmd/pg-partition/fold.go`**

```go
type FoldResult struct {
    Name          string
    From, Through time.Time
    Rows, Users   int64
    Delta         int64
    Skipped       string // non-empty when ledger_folded already had it
}

// foldExpired folds every ledger child whose upper bound is at or before cutoff
// into ledger_opening. Each partition is one transaction that DETACHes it and adds
// its per-user totals in the same commit, so accounts.balance == ledger_opening +
// SUM(ledger) holds at every instant, not merely between runs. Idempotent via
// ledger_folded's primary key.
func foldExpired(ctx context.Context, pool *pgxpool.Pool, schema string, cutoff time.Time, dry bool) ([]FoldResult, error)

// dropFolded drops every detached child already recorded in ledger_folded,
// including orphans left by a crash between the fold's COMMIT and its DROP.
func dropFolded(ctx context.Context, pool *pgxpool.Pool, schema string, dry bool) (dropped []string, err error)
```

The fold transaction:

```sql
BEGIN;
  ALTER TABLE ledger DETACH PARTITION ledger_20260701;

  INSERT INTO ledger_opening (user_id, delta, through_ts)
  SELECT user_id, SUM(delta), MAX(ts) FROM ledger_20260701 GROUP BY user_id
  ON CONFLICT (user_id) DO UPDATE
    SET delta      = ledger_opening.delta + EXCLUDED.delta,
        through_ts = GREATEST(ledger_opening.through_ts, EXCLUDED.through_ts);

  INSERT INTO ledger_folded (name, from_ts, through_ts, rows, delta, folded_at)
  SELECT 'ledger_20260701', $1, $2, COUNT(*), COALESCE(SUM(delta), 0), $3
    FROM ledger_20260701;
COMMIT;
```

**`cmd/pg-partition/verify.go`**

```go
type Mismatch struct {
    UserID           string
    Balance, Derived int64
}

// reconcile returns every user whose materialized balance disagrees with
// ledger_opening + SUM(ledger). It gates the drop: history is only destroyed once
// it has proved it agrees with the balances that outlive it.
func reconcile(ctx context.Context, pool *pgxpool.Pool) ([]Mismatch, error)
```

One statement, so it reads one snapshot — a fold committing between two statements would otherwise
show as a phantom mismatch:

```sql
SELECT a.user_id, a.balance,
       COALESCE(o.delta, 0) + COALESCE(l.total, 0) AS derived
  FROM accounts a
  LEFT JOIN ledger_opening o ON o.user_id = a.user_id
  LEFT JOIN (SELECT user_id, SUM(delta) AS total FROM ledger GROUP BY user_id) l
         ON l.user_id = a.user_id
 WHERE a.balance <> COALESCE(o.delta, 0) + COALESCE(l.total, 0)
```

Also check the reverse direction — a `ledger`/`ledger_opening` user with no `accounts` row is
money with no owner, and the `LEFT JOIN` from `accounts` will not find it.

**Pass order in `main.go`**: advisory lock → `ensureRegistered` → `Maintain` → `foldExpired` →
`reconcile` → `dropFolded` (only if clean) → release. Print a one-line summary per fold and per
drop; launchd captures stdout.

## Tests

`cmd/pg-partition/fold_test.go`, skipping without `TEST_DATABASE_URL`.

- **`TestFoldKeepsReconcileClean`** — seed a randomized workload across several days through the
  real `Credit`/`Grant`/`Spend`/`Transfer`, snapshot every `Balance()`, fold the oldest day, then
  assert `reconcile` is empty and every balance is unchanged.
- **`TestFoldIsIdempotent`** — run the pass twice; the second folds nothing and changes nothing.
- **`TestFoldSurvivesCrashAfterCommit`** — commit a fold transaction, skip the `DROP`, re-run the
  pass. The orphan is dropped, `ledger_opening` is not double-counted, `reconcile` stays clean.
- **`TestDropIsGatedOnReconcile`** — corrupt one `accounts.balance`, run the pass, assert a non-zero
  exit and that **no** partition was dropped.
- **`TestFoldOfEmptyPartitionIsRecorded`** — a day with no rows still gets a `ledger_folded` row, or
  it is re-folded forever.
- **`TestConcurrentPassesSerialize`** — two passes at once; one takes the advisory lock, the other
  reports contention and exits cleanly rather than half-running.

## Acceptance

- `mise run test:all` green.
- Against a restored production copy with `-retention` set short enough to force folds: `reconcile`
  clean, every `Balance()` identical before and after, `SUM(ledger_opening.delta) +
  SUM(ledger.delta) = SUM(accounts.balance)`, and `ledger_folded` lists exactly the dropped days.
- `\dt ledger_*` shows no detached orphans afterwards.
- Run twice: the second is a no-op.
