-- See the Postgres twin (store/postgres/migrations/00004_ledger_refs_and_opening.sql)
-- for the full reasoning, and docs/adr/0002-ledger-retention-and-partitioning.md
-- for the epic.
--
-- SQLite has no partitions and gets no retention, so ledger_opening and
-- ledger_folded will never gain a row here. They exist so both backends keep the
-- same table list, which is what lets cmd/store-migrate stay a table-for-table
-- copier with no special cases -- the same reason 00002 gave for creating
-- accounts on a backend that (then) never wrote to it.
--
-- ledger_refs is different: it is behavioral. Credit's idempotency is
-- conformance-tested on both backends, so the mechanism has to be the same one
-- on both, and SQLite's ledger_ref index goes away with Postgres's.

-- +goose Up

CREATE TABLE IF NOT EXISTS ledger_refs (
	ref     TEXT    PRIMARY KEY,
	user_id TEXT    NOT NULL,
	ts      INTEGER NOT NULL
);

INSERT INTO ledger_refs (ref, user_id, ts)
SELECT ref, user_id, MIN(ts) FROM ledger WHERE ref IS NOT NULL GROUP BY ref, user_id
ON CONFLICT(ref) DO NOTHING;

DROP INDEX IF EXISTS ledger_ref;

-- Created, never written. On SQLite the invariant accounts.balance ==
-- ledger_opening + SUM(ledger) holds with the first term permanently zero.
CREATE TABLE IF NOT EXISTS ledger_opening (
	user_id    TEXT    PRIMARY KEY,
	delta      INTEGER NOT NULL,
	through_ts INTEGER NOT NULL
);

-- Created, never written: there are no partitions here to fold.
CREATE TABLE IF NOT EXISTS ledger_folded (
	name       TEXT    PRIMARY KEY,
	from_ts    INTEGER NOT NULL,
	through_ts INTEGER NOT NULL,
	rows       INTEGER NOT NULL,
	delta      INTEGER NOT NULL,
	folded_at  INTEGER NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS ledger_folded;
DROP TABLE IF EXISTS ledger_opening;
DROP TABLE IF EXISTS ledger_refs;
CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL;
