# Store portability + money fixes

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: nothing · Unblocks: nothing (can land first or in parallel)

## Summary

Fix two defects that already exist in the SQLite store before any Postgres code lands: a
`Leaderboard` query that is invalid Postgres SQL, and transactions that claim to be immediate but
aren't. Both are real today; doing them first means the Postgres backend is a port of correct code
rather than a port plus a bug hunt.

## Decisions

- **Fix in place, on the current `store` package.** No refactor here — this issue must be reviewable
  as a pure bug fix and cherry-pickable on its own.
- **`COLLATE` is deferred to issue 06.** SQLite's byte comparison is the reference behavior; the
  Postgres side adopts `COLLATE "C"` to match it. Nothing to change here.

## Work breakdown

1. **`Leaderboard` — `store/points.go:173`.** `HAVING bal > 0` references the `SELECT` alias `bal`.
   SQLite permits it; Postgres does not. Also, `GROUP BY l.user_id` alone is invalid in Postgres
   because `u.login` / `u.display` are neither grouped nor aggregated.

   ```sql
   SELECT l.user_id, u.login, u.display, SUM(l.delta) AS bal
   FROM ledger l JOIN users u ON u.user_id = l.user_id
   GROUP BY l.user_id, u.login, u.display
   HAVING SUM(l.delta) > 0
   ORDER BY bal DESC, u.display ASC
   LIMIT ?
   ```

   `ORDER BY` may keep the alias — that one is legal in both dialects.

2. **`BEGIN IMMEDIATE` — `store/store.go:30` (`Open`).** `Grant`, `Spend` and `Transfer` each say
   "one immediate transaction", but `db.Begin()` starts a *deferred* transaction: the read takes a
   shared lock and the write then attempts an upgrade, which `busy_timeout` does **not** rescue —
   SQLite returns `SQLITE_BUSY` immediately on a failed upgrade rather than retrying. Open the
   database with `_txlock=immediate` so every transaction takes the write lock up front:

   ```go
   dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)"
   ```

   Verify the pinned `modernc.org/sqlite` honours `_txlock`; if it doesn't, the fallback is a
   `db.Conn` with an explicit `BEGIN IMMEDIATE`. Keep the `PRAGMA journal_mode = WAL` exec. Update
   the comment at `store/store.go:35-36` to say what is actually happening.

3. **Correct the stale comments** on `Grant`/`Spend`/`Transfer` so they describe the guarantee the
   code now has, and note that the guarantee comes from SQLite's single writer — flagging that a
   multi-writer backend will need explicit locking (forward reference to issue 06).

## Tests

- **`store/points_test.go`** — new case: a user whose deltas sum to exactly `0`, and one summing
  negative, are both absent from `Leaderboard`; a user at `+1` is present. This pins the `HAVING`
  fix and is the case the conformance suite later inherits.
- New case: `Leaderboard` orders by balance descending, tie-broken by display ascending.
- **`store/store_test.go`** — a concurrent `Spend` smoke test (a handful of goroutines against one
  balance) asserting no error and no negative final balance. Cheap insurance that `_txlock` didn't
  regress anything.
- `go test -race ./store/...` clean.

## Out of scope

- Any package moves, interface extraction, or new tables.
- The `accounts` lock table — SQLite doesn't need it (issue 05 adds it for schema parity).

## References

- `store/points.go` (`Leaderboard`, `Grant`, `Spend`, `Transfer`)
- `store/store.go` (`Open`, the pragma loop)
- `store/points_test.go`, `store/store_test.go` (`openTemp`)
