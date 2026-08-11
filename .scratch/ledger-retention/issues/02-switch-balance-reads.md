# Read balances from accounts.balance

Status: done (2026-08-11)
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 01 · Unblocks: 03

## Summary

Flip `Balance`, `balanceTx` and `Leaderboard` to read the column issue 01 built. After this the
ledger is history, not the answer to "what do I have".

## Decisions

- **The whole issue rests on `MaterializedBalanceMatchesLedger`.** That is why it is a separate
  commit from issue 01: the column soaks under test before anything reads it, and this change is a
  one-line-per-query diff that is easy to revert if the soak went badly.
- **`balanceTx` gets the same query text as `Balance`.** It is inside the account lock and would be
  safe either way, but two texts drift and one does not.
- **`Leaderboard` stops touching `ledger` entirely.** `accounts JOIN users`, `WHERE balance > 0`,
  served by the partial index from issue 01. This is the query that would otherwise aggregate every
  row in every partition.
- **Semantics do not change.** The join still omits users with no `users` row. `WHERE a.balance > 0`
  is the old `HAVING SUM(l.delta) > 0` — an `accounts` row that never earned anything is 0 and is
  excluded either way. `COLLATE "C"` stays on the Postgres tie-break.
- **`ledgerTotals` in the cutover tool becomes balance-aware here, not later.** The moment balance
  lives on `accounts`, `SUM(ledger.delta)` stops being "the total money" — and a copy that dropped
  every `accounts.balance` would still pass verification on row counts and ledger sums. This is the
  single highest-value line in the epic.

## Work breakdown

1. **`store/postgres/points.go`**

   ```sql
   -- Balance
   SELECT COALESCE((SELECT balance FROM accounts WHERE user_id = $1), 0)

   -- balanceTx (caller holds the lock)
   SELECT balance FROM accounts WHERE user_id = $1

   -- Leaderboard
   SELECT a.user_id, u.login, u.display, a.balance
     FROM accounts a JOIN users u ON u.user_id = a.user_id
    WHERE a.balance > 0
    ORDER BY a.balance DESC, u.display COLLATE "C" ASC
    LIMIT $1
   ```

   `balanceTx` must tolerate no row: `Grant` can be the first thing that ever happens to a user, and
   `lockAccount` runs first so the row will exist — but keep the `COALESCE` shape in `Balance`,
   which has no lock and no guarantee.

2. **`store/sqlite/points.go`** — the same three, `?` placeholders, no `COLLATE "C"` (SQLite
   compares bytes already; see `00001`'s note).

3. **Package comments in both `points.go`** — the opening paragraph in each says a balance is
   `SUM(delta)`. Replace with the real model: `accounts.balance` is the balance, the ledger is the
   history it must reconcile to, and `cmd/pg-partition` is what proves it. Cross-reference
   `docs/adr/0002`.

4. **`cmd/store-migrate/verify.go`** — `ledgerTotals` returns the materialized total alongside the
   ledger total and the max id:

   ```go
   // SUM(ledger.delta) alone stopped being the total the moment balance moved onto
   // accounts and retention could drop the rows it sums. A copy that lost every
   // accounts.balance would otherwise pass verification with matching row counts
   // and matching ledger sums.
   `SELECT COALESCE((SELECT SUM(balance) FROM accounts), 0),
           COALESCE((SELECT SUM(delta) FROM ledger), 0),
           COALESCE((SELECT MAX(id) FROM ledger), 0)`
   ```

   Compare the materialized total across the copy *and* assert it equals the ledger total on each
   side. (`ledger_opening` does not exist until issue 03; add its term there.)

## Tests

- **Every existing points and concurrency case passes unmodified.** They were written against
  `SUM(ledger)` semantics; passing against the column is the proof the two agree.
  `LeaderboardExcludesZeroAndNegative` and `LeaderboardOrderAndTieBreak` are the ones that pin the
  rewritten query.
- **`MaterializedBalanceMatchesLedger` keeps running** — it now asserts the read path and the
  ledger agree, which is a different (and still useful) claim than before.
- **New: `LeaderboardMatchesBalances`** — for every row the leaderboard returns, `Balance(user_id)`
  returns the same number. Cheap, and it catches a join that silently changes the aggregate.
- **Round trip**: `cmd/store-migrate` pg → sqlite → pg, `-verify-only` passes; then hand-corrupt one
  `accounts.balance` in the destination and assert verification fails.

## Acceptance

- `mise run test:all` green.
- `EXPLAIN` on the leaderboard query shows `accounts_balance` used and no `ledger` scan.
- Against a restored production copy, `Leaderboard(10)` returns byte-identical output before and
  after the change.
