-- accounts gains a materialized balance. See the Postgres twin
-- (store/postgres/migrations/00003_materialized_balance.sql) for why a decision
-- 00002 and ADR-0001 both made deliberately is being reversed, and
-- docs/adr/0002-ledger-retention-and-partitioning.md for the full argument.
--
-- 00002 says "SQLite creates accounts and never writes to it". That stops being
-- true here. The *lock* is still unnecessary — SQLite admits one writer at a time
-- — but the *balance* is behavioral: Balance, Credit, Grant, Spend, Transfer and
-- Leaderboard all read or write it, and all six are conformance-tested. A table
-- that exists on both backends but only carries data on one would make the suite
-- test two different programs.
--
-- SQLite has no partitions and gets no retention, so it never gains a
-- ledger_opening row and its balance is always exactly SUM(ledger). That is the
-- same invariant the Postgres side maintains, with one term always zero.

-- +goose Up
ALTER TABLE accounts ADD COLUMN balance INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS accounts_balance ON accounts (balance DESC) WHERE balance > 0;

-- Far more inserts than updates: accounts has a row per user who moved money
-- through a locking path, which on SQLite is nobody, while ledger has rows for
-- everyone who ever accrued.
INSERT INTO accounts (user_id, balance, created_at, updated_at)
SELECT user_id, SUM(delta), 0, 0 FROM ledger GROUP BY user_id
ON CONFLICT(user_id) DO UPDATE SET balance = excluded.balance;

-- +goose Down
DROP INDEX IF EXISTS accounts_balance;
ALTER TABLE accounts DROP COLUMN balance;
