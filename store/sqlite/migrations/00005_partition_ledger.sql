-- Deliberately a no-op. SQLite has no table partitioning and gets no retention,
-- so its ledger stays one ordinary table and never gains ts_at.
--
-- The file exists so both dialects stay on the same schema version. They are
-- meant to describe the same schema, cmd/store-migrate prints the two side by
-- side, and both migrate_test.go files assert the same wantSchemaVersion --
-- letting Postgres run ahead would make every later comparison an exercise in
-- remembering which dialect is which.
--
-- The Postgres twin (store/postgres/migrations/00005_partition_ledger.sql) does
-- the real work. See docs/adr/0002-ledger-retention-and-partitioning.md for why
-- retention is Postgres-only: SQLite is the dev/test backend, and a
-- money-touching prune path that only ever runs under test is one that is never
-- really tested.

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
