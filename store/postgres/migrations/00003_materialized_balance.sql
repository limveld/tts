-- The Postgres twin of store/sqlite/migrations/00003_materialized_balance.sql.
--
-- accounts stops being a pure lock token here. 00002 says it deliberately has no
-- balance column "so the ledger stays the single source of truth and nothing
-- derived can drift", and ADR-0001 says the SUM is free at 114 users. Both were
-- right. What changed is that the ledger is about to be partitioned by day with a
-- 365-day horizon, and Balance/Leaderboard filter on user_id and never on time —
-- so there is nothing to prune on and every balance read would fan out across
-- ~370 children. Retention then makes the rows being summed impermanent, which is
-- the deeper problem: a balance derived from history cannot outlive it.
--
-- The drift risk 00002 names is real and is answered by construction rather than
-- by abstinence:
--   * balance is written in the same transaction as the ledger row it reflects,
--     by applyDelta, which every money path calls;
--   * the conformance suite asserts balance == SUM(ledger) after a randomized
--     workload, on both backends;
--   * cmd/pg-partition refuses to drop any history until it has proved
--     balance == ledger_opening + SUM(ledger) for every user.
--
-- See docs/adr/0002-ledger-retention-and-partitioning.md.
--
-- Nothing reads this column yet — that is migration 00003's whole point. The read
-- paths switch over in a separate commit, once the column has proved itself.

-- +goose Up
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS balance BIGINT NOT NULL DEFAULT 0;

-- Leaderboard's entire query once the reads switch: accounts JOIN users WHERE
-- balance > 0. Partial, because a leaderboard never asks about the zero and
-- negative balances and they are most of the table after a few years.
CREATE INDEX IF NOT EXISTS accounts_balance ON accounts (balance DESC) WHERE balance > 0;

-- accounts holds a row per user who has moved money through a locking path;
-- ledger holds rows for everyone who ever accrued. In production that is 2 rows
-- against 114 users, so this inserts far more than it updates — INSERT ... ON
-- CONFLICT rather than a bare UPDATE for exactly that reason.
--
-- created_at/updated_at stay 0 for the rows this invents. They mean "when did
-- this account row appear", and backdating them to now() would claim these
-- accounts were created at migration time, which is the one thing we know is
-- false.
INSERT INTO accounts (user_id, balance, created_at, updated_at)
SELECT user_id, SUM(delta), 0, 0 FROM ledger GROUP BY user_id
ON CONFLICT (user_id) DO UPDATE SET balance = EXCLUDED.balance;

-- +goose Down
DROP INDEX IF EXISTS accounts_balance;
ALTER TABLE accounts DROP COLUMN IF EXISTS balance;
