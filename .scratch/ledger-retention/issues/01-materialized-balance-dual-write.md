# accounts.balance, maintained but not yet read

Status: done (2026-08-11)
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: — · Unblocks: 02

> **Second-highest-risk issue in the epic**, after 04. It introduces a derived value over real
> currency — exactly what [ADR-0001](../../../docs/adr/0001-postgres-backend.md) avoided. The
> mitigation is that nothing reads it until issue 02, and a conformance test has to prove it agrees
> with `SUM(ledger)` first.

## Summary

Add `accounts.balance`, backfill it, and maintain it in the same transaction as every ledger row —
on **both** backends. Reads still use `SUM(ledger)`. The point of the issue is to get the column
correct and proven while it is still ignorable.

## Decisions

- **Dual-write, then switch.** The column proves itself for one migration before anything depends
  on it. If it drifts, `MaterializedBalanceMatchesLedger` fails and nobody's `!marks` was ever
  wrong.
- **`applyDelta`, not "update the balance somewhere".** Postgres's `touchAccount` is renamed and
  gains the balance clause, because every call site that touches the account is exactly a call site
  that changes the balance. One helper, one place to get it wrong.
- **`ensureAccount` is extracted from `lockAccount`.** `Credit` needs the row to exist but does not
  need to lock it for reading. Splitting the upsert out avoids giving the hot accrual path a
  `FOR UPDATE` it does not need.
- **SQLite starts writing to `accounts`.** Today it never does — `00002`'s comment says so in as
  many words, and that comment becomes false here. The lock is still unnecessary (one writer), but
  the *balance* is behavioral and conformance-tested, so it has to be real on both sides. Say so in
  `00003`'s header, since the reader will have just read `00002` claiming the opposite.
- **`Credit` becomes transactional on both backends.** It is currently a single autocommit `Exec`
  on each side. The ledger row and the balance must commit together or they can disagree, so it
  gets a transaction. Order inside it: ledger row, then `applyDelta` **last**, so the row lock the
  blind `UPDATE` takes is held as briefly as possible.
- **The backfill creates rows, it does not only update them.** `accounts` holds 2 rows in
  production; 114 users have ledger history. `ON CONFLICT DO UPDATE` over a `GROUP BY` of the
  ledger is the whole migration.

## Work breakdown

1. **`store/postgres/migrations/00003_materialized_balance.sql`** and its SQLite twin (real work in
   both — this is not a no-op migration).

   ```sql
   -- +goose Up
   -- accounts stops being a pure lock token here. 00002 says it has no balance
   -- column "so there is no derived value that can drift"; that was the right call
   -- when a balance was one indexed SUM. Daily partitioning of the ledger makes
   -- that SUM fan out across ~370 children, and retention makes the rows it sums
   -- impermanent. See docs/adr/0002. The drift risk is real and is answered by
   -- construction: balance is written in the same transaction as the ledger row,
   -- and cmd/pg-partition refuses to drop history until it has proved
   -- balance == ledger_opening + SUM(ledger) for every user.
   ALTER TABLE accounts ADD COLUMN balance BIGINT NOT NULL DEFAULT 0;

   -- Leaderboard's entire query once issue 02 lands.
   CREATE INDEX IF NOT EXISTS accounts_balance ON accounts (balance DESC) WHERE balance > 0;

   -- accounts holds a row per *money mover*; the ledger holds rows for everyone
   -- who ever accrued. So this inserts far more rows than it updates.
   INSERT INTO accounts (user_id, balance, created_at, updated_at)
   SELECT user_id, SUM(delta), 0, 0 FROM ledger GROUP BY user_id
   ON CONFLICT (user_id) DO UPDATE SET balance = EXCLUDED.balance;
   ```

   SQLite: `ALTER TABLE accounts ADD COLUMN balance INTEGER NOT NULL DEFAULT 0`, the same partial
   index, and the same backfill written as `ON CONFLICT(user_id) DO UPDATE`.

2. **`store/postgres/points.go`**
   - Extract `ensureAccount(ctx, tx, userID, now)` — the `INSERT INTO accounts … ON CONFLICT DO
     NOTHING` half of `lockAccount`. `lockAccount` calls it and keeps its retry loop.
   - `touchAccount` → `applyDelta(ctx, tx, userID, delta, now)`:
     ```go
     `UPDATE accounts SET balance = balance + $2, updated_at = $3 WHERE user_id = $1`
     ```
   - `Grant`: `touchAccount(…)` → `applyDelta(…, applied, …)`. It must be `applied`, not `delta` —
     the clamped claw-back writes a clamped ledger row and the two have to move together.
   - `Spend`: → `applyDelta(…, -amount, …)`. `Transfer`: two calls, `-amount` and `+amount`.
   - `Credit`: wrap both branches in a transaction; `ensureAccount`, insert, `applyDelta(…, amount,
     …)`, commit. Keep the `ON CONFLICT (ref) WHERE ref IS NOT NULL` predicate exactly as-is —
     `ledger_refs` does not arrive until issue 03 — and keep returning `credited=false` without
     applying a delta when it conflicts.

3. **`store/sqlite/points.go`** — the same five methods. SQLite has no `lockAccount`/`touchAccount`
   to rename, so add `ensureAccount` and `applyDelta` as the twins of the Postgres helpers, taking
   `*sql.Tx`. `Grant`/`Spend`/`Transfer` already run in transactions and just gain the two calls.
   `Credit` gains one. Update the file's package comment: "a balance is SUM(delta)" is what is
   changing.

4. **`store/storetest/invariants.go`** — a third entry point beside `Run` and `RunConcurrent`,
   because `storetest.New` hands back only the `Store` interface and this case has to read a column
   no interface method exposes:

   ```go
   // NewWithDB builds an isolated store and hands back a handle to the same
   // database. Only the invariant suite gets one: every other case must express
   // itself through the contract, or it is testing an implementation rather than
   // a behavior.
   type NewWithDB func(t *testing.T) (Store, *sql.DB)

   func RunInvariants(t *testing.T, newStore NewWithDB)
   ```

   Wire it into `store/postgres/conformance_test.go` and `store/sqlite/conformance_test.go`. For
   Postgres the handle must be the temp-schema one (`search_path` already set by
   `storetest/pg.go`), not a fresh connection to `public`.

5. **`cmd/store-migrate/main.go`** — `accounts` gains `balance` in its column list, or a rollback
   drops everyone's balance to 0.

6. **`store/{postgres,sqlite}/migrate_test.go`** — `wantSchemaVersion` → 3 in both.

## Tests

- **`MaterializedBalanceMatchesLedger`** (new, both dialects, the gate for issue 02). Drive a
  randomized but seeded workload over ~50 users: `Credit` with and without refs (including repeats
  of the same ref), `Grant` both positive and negative-past-zero (the clamp), `Spend` both
  affordable and refused, `Transfer` in both directions including a self-transfer. Then for every
  user assert `accounts.balance == SUM(ledger.delta)`.
- **Every existing points case passes unmodified.** Reads have not moved yet, so any change in
  behavior here is a bug in the write paths.
- **Postgres concurrency cases still pass** — `applyDelta` on a row already held by `lockAccount`
  is the same lock, and `Credit`'s blind `UPDATE` takes a row lock it holds only to commit. Watch
  `ConcurrentCreditWithSameRefCreditsOnce` in particular: `Credit` is transactional now, so 16
  goroutines serialize on one `accounts` row where they previously raced an index.
- **Migration test**: after `00003` on a fixture with ledger history and no `accounts` rows, every
  ledger user has an `accounts` row and `balance` equals their `SUM`.

## Acceptance

- `mise run test:all` green on both backends.
- `SELECT count(*) FROM accounts a WHERE a.balance <> (SELECT COALESCE(SUM(delta),0) FROM ledger WHERE user_id = a.user_id)`
  returns 0 against a restored copy of production.
- `SELECT count(*) FROM accounts` is 114, not 2.
- Nothing reads `accounts.balance` yet — `grep` for it outside `points.go` write paths, the
  migration and the test returns nothing.
