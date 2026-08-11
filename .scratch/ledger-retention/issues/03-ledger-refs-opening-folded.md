# ledger_refs, ledger_opening, ledger_folded — and idempotency off the ledger

Status: done (2026-08-11)
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 02 · Unblocks: 04

## Summary

Create the three retention-support tables in both dialects and move channel-point idempotency off
`ledger` into `ledger_refs`. Nothing is partitioned yet; this is the last issue that leaves the
schema shape ordinary.

## Decisions

- **Idempotency is a property of the redemption id, not of the ledger row it produced.** So it has
  to outlive that row. Today `ledger_ref` is `UNIQUE (ref) WHERE ref IS NOT NULL` on the ledger
  itself, which means deleting an old ledger row silently re-arms an old redemption id. That is a
  latent bug independent of partitioning, and this issue fixes it.
- **It has to move before issue 04, not because of it.** On a partitioned `ledger` a unique index
  must include the partition key, degrading the guarantee to "at most once *per day*" — and the
  failure mode lands exactly where Twitch's re-delivery window does. But the fix stands on its own.
- **Both dialects drop their `ledger_ref` index.** SQLite's `Credit` currently leans on it via
  `INSERT OR IGNORE`. Keeping the index there as a second mechanism is precisely the drift the
  conformance suite exists to prevent, and once a ledger row can be dropped the two mechanisms
  disagree.
- **`ledger.ref` stays as a column.** Provenance, and it keeps `cmd/store-migrate` a symmetric
  table-for-table copier.
- **`ledger_opening` is an audit anchor, not a balance.** Balance lives on `accounts` as of issue
  02. This holds the per-user total of history that has been *dropped*, so
  `accounts.balance == ledger_opening + SUM(ledger)` stays checkable forever. Nothing reads it for
  money.
- **`ledger_folded` is bookkeeping and SQLite never writes it** — the `accounts`-in-`00002`
  precedent, now that `accounts` itself no longer qualifies. Both backends keep the same table list
  so the copier stays special-case-free.

## Work breakdown

1. **`store/postgres/migrations/00004_ledger_refs_and_opening.sql`** + SQLite twin (real work in
   both):

   ```sql
   -- +goose Up
   -- Idempotency for channel-point redemptions, moved off ledger. It lives in its
   -- own table because the guarantee is about the redemption id and has to survive
   -- the ledger row: once retention can drop old rows, a ref-scoped unique index on
   -- ledger would re-arm every redemption whose row aged out.
   CREATE TABLE IF NOT EXISTS ledger_refs (
       ref     TEXT   PRIMARY KEY,
       user_id TEXT   NOT NULL,
       ts      BIGINT NOT NULL
   );

   INSERT INTO ledger_refs (ref, user_id, ts)
   SELECT ref, user_id, MIN(ts) FROM ledger WHERE ref IS NOT NULL GROUP BY ref, user_id
   ON CONFLICT (ref) DO NOTHING;

   DROP INDEX IF EXISTS ledger_ref;

   -- The audit anchor. Not a balance -- that is accounts.balance as of 00003/issue
   -- 02. This is the part of the history that has been dropped, kept so that
   -- accounts.balance == ledger_opening + SUM(ledger) stays provable after
   -- retention has run.
   CREATE TABLE IF NOT EXISTS ledger_opening (
       user_id    TEXT   PRIMARY KEY,
       delta      BIGINT NOT NULL,
       through_ts BIGINT NOT NULL
   );

   -- Which partitions have been folded. The primary key is the idempotency: a fold
   -- that crashed after COMMIT is indistinguishable from one that finished. SQLite
   -- creates this and never writes to it -- there is no retention there -- so that
   -- both backends keep the same table list and the migrate tool stays a
   -- table-for-table copier.
   CREATE TABLE IF NOT EXISTS ledger_folded (
       name       TEXT   PRIMARY KEY,
       from_ts    BIGINT NOT NULL,
       through_ts BIGINT NOT NULL,
       rows       BIGINT NOT NULL,
       delta      BIGINT NOT NULL,
       folded_at  BIGINT NOT NULL
   );
   ```

   The back-fill's `GROUP BY ref, user_id` with `ON CONFLICT DO NOTHING` is deliberate: if the same
   ref somehow exists against two users, take one rather than failing the migration. Log-worthy but
   not fatal. (Production has 46 ref rows; check for that case before assuming zero.)

2. **`store/postgres/points.go`** — `Credit`'s ref branch:

   ```go
   res, err := tx.ExecContext(ctx,
       `INSERT INTO ledger_refs (ref, user_id, ts) VALUES ($1, $2, $3)
        ON CONFLICT (ref) DO NOTHING`, ref, userID, now)
   if n, _ := res.RowsAffected(); n == 0 {
       return false, nil   // already applied; the deferred Rollback is a no-op
   }
   // … ensureAccount, INSERT INTO ledger, applyDelta, Commit
   ```

   The ledger insert loses its `ON CONFLICT (ref) WHERE ref IS NOT NULL` clause and the comment
   above it explaining the predicate-repetition requirement — both are obsolete.

3. **`store/sqlite/points.go`** — the identical two-statement path; `INSERT OR IGNORE INTO
   ledger_refs`, then check `RowsAffected`. The `INSERT OR IGNORE` on `ledger` becomes a plain
   `INSERT`.

4. **`cmd/store-migrate/main.go`** — three new entries in `tables`. Order matters only for
   readability, but put `ledger_refs`/`ledger_opening` before `ledger` and `ledger_folded` after,
   so the file reads in dependency order even though there are no foreign keys.

5. **`cmd/store-migrate/verify.go`** — add the `ledger_opening` term to the total from issue 02:
   `SUM(accounts.balance)` must equal `SUM(ledger.delta) + SUM(ledger_opening.delta)`.

6. **`store/{postgres,sqlite}/migrate_test.go`** — `wantSchemaVersion` → 4; three tables added to
   the fresh-schema list.

## Tests

- **`CreditIdempotentRef` and `ConcurrentCreditWithSameRefCreditsOnce` pass unmodified.** This is
  the acceptance criterion for the whole swap. The concurrent one works because `ON CONFLICT DO
  NOTHING` against a key held by an *uncommitted* transaction blocks on the speculative-insertion
  token rather than skipping: exactly one goroutine gets `n=1`, the rest block and then return
  `credited=false`.
- **New `CreditRefRejectedAfterItsLedgerRowIsGone`** — credit with ref `r`, delete the ledger row
  directly (standing in for a dropped partition), credit with `r` again, assert `credited=false`
  and the balance did not move. This is the latent bug; it fails before the change.
- **Deadlock check**: `Credit` now holds a `ledger_refs` row lock and an `accounts` row lock in one
  transaction, while `Transfer` holds two `accounts` locks in sorted order. `Credit` only ever waits
  on its own single `accounts` row and on `ledger_refs` (which nothing else locks), so no cycle is
  constructible — `BidirectionalTransfersDoNotDeadlock` plus the concurrent-credit case should hold
  it, but run them with `-count=20`.
- **Migration test**: a fixture with duplicate-ref ledger rows migrates without error and yields one
  `ledger_refs` row per distinct ref.

## Acceptance

- `mise run test:all` green, concurrency cases at `-count=20`.
- Against a restored production copy: `SELECT count(*) FROM ledger_refs` is 46, and
  `SELECT count(DISTINCT ref) FROM ledger WHERE ref IS NOT NULL` matches it.
- `\d ledger` no longer lists `ledger_ref`.
