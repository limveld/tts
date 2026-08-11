-- The Postgres twin of store/sqlite/migrations/00004_ledger_refs_and_opening.sql.
--
-- Three tables that make the ledger expirable. None of them is read for money:
-- accounts.balance is the balance as of 00003. See
-- docs/adr/0002-ledger-retention-and-partitioning.md.

-- +goose Up

-- Idempotency for channel-point redemptions, moved off ledger.
--
-- It lives in its own table because the guarantee is about the redemption id and
-- has to outlive the ledger row it produced. Today's ledger_ref index is on the
-- ledger itself, so once retention can drop old rows, every redemption whose row
-- aged out would be re-armed -- a latent bug independent of partitioning.
--
-- It also cannot stay where it is: a partitioned table's unique index must
-- include the partition key, so on a ledger partitioned by day the guarantee
-- would degrade to "at most once per day", and the failure lands exactly where
-- Twitch's re-delivery window does. This table is deliberately not partitioned.
CREATE TABLE IF NOT EXISTS ledger_refs (
	ref     TEXT   PRIMARY KEY,
	user_id TEXT   NOT NULL,
	ts      BIGINT NOT NULL
);

-- GROUP BY ref, user_id with ON CONFLICT DO NOTHING rather than a bare SELECT:
-- if the same ref somehow exists against two users, take one rather than failing
-- the migration. Production has 46 ref rows and no such case; this is about not
-- discovering one during a cutover.
INSERT INTO ledger_refs (ref, user_id, ts)
SELECT ref, user_id, MIN(ts) FROM ledger WHERE ref IS NOT NULL GROUP BY ref, user_id
ON CONFLICT (ref) DO NOTHING;

DROP INDEX IF EXISTS ledger_ref;

-- The audit anchor. Not a balance -- that is accounts.balance -- and nothing
-- reads it to answer "what do I have". It holds the per-user total of history
-- that has been dropped, so that
--
--     accounts.balance == ledger_opening.delta + SUM(ledger.delta)
--
-- stays provable for every user after retention has run. cmd/pg-partition
-- refuses to drop a partition until that check comes back clean, which is what
-- makes the materialized balance something the tooling proves rather than
-- something the schema hopes for.
CREATE TABLE IF NOT EXISTS ledger_opening (
	user_id    TEXT   PRIMARY KEY,
	delta      BIGINT NOT NULL,
	through_ts BIGINT NOT NULL   -- newest ts folded in; observability only
);

-- Which partitions have been folded into ledger_opening. The primary key is the
-- idempotency: a fold that crashed after COMMIT is indistinguishable from one
-- that finished, so the next run skips it and drops the orphan rather than
-- double-counting it.
--
-- Deliberately ours rather than partman.partitions.status: coupling a money
-- invariant to another library's bookkeeping means a library upgrade can change
-- what the money means.
CREATE TABLE IF NOT EXISTS ledger_folded (
	name       TEXT   PRIMARY KEY,   -- partition name, e.g. 'ledger_20260701'
	from_ts    BIGINT NOT NULL,
	through_ts BIGINT NOT NULL,
	rows       BIGINT NOT NULL,
	delta      BIGINT NOT NULL,
	folded_at  BIGINT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS ledger_folded;
DROP TABLE IF EXISTS ledger_opening;
DROP TABLE IF EXISTS ledger_refs;
CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL;
