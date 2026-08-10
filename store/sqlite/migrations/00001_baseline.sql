-- The baseline is the schema as it stood before there were any migrations, copied
-- statement for statement out of Open's old inline schema slice. It is frozen:
-- do not tidy it, do not fold later changes into it.
--
-- The reason is that the live bot.db has this schema and no version history at
-- all. Every statement here is IF NOT EXISTS, so applying this file to that
-- database is a genuine no-op that writes only the goose version row — no
-- stamping trick, no "if the tables already exist then pretend" branch. Applying
-- it to an empty file produces exactly the same schema. Rewriting it to be
-- prettier would break the first of those two properties.

-- +goose Up
CREATE TABLE IF NOT EXISTS commands (
	name     TEXT PRIMARY KEY,
	response TEXT NOT NULL,
	cooldown INTEGER NOT NULL DEFAULT 0,
	min_role TEXT NOT NULL DEFAULT 'everyone',
	count    INTEGER NOT NULL DEFAULT 0
);

-- The loyalty-points ("marks") economy: an identity table and an append-only
-- ledger (balance = SUM(delta)). See points.go.
CREATE TABLE IF NOT EXISTS users (
	user_id   TEXT PRIMARY KEY,
	login     TEXT NOT NULL,
	display   TEXT NOT NULL,
	last_seen INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ledger (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id TEXT NOT NULL,
	delta   INTEGER NOT NULL,
	reason  TEXT NOT NULL,
	ref     TEXT,
	ts      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS ledger_user ON ledger(user_id);

-- Idempotent channel-point crediting: a redemption id credits at most once.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL;

-- Small key/value store for runtime toggles (the free/paid charge mode and the
-- depth widget's totals).
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- Wordle win tally (one row per solver). See wordle.go.
CREATE TABLE IF NOT EXISTS wordle_wins (
	user_id TEXT PRIMARY KEY,
	login   TEXT NOT NULL,
	display TEXT NOT NULL,
	wins    INTEGER NOT NULL DEFAULT 0
);

-- Connections completion tally (one row per player who landed the final group of
-- a puzzle). See connections.go.
CREATE TABLE IF NOT EXISTS connections_wins (
	user_id TEXT PRIMARY KEY,
	login   TEXT NOT NULL,
	display TEXT NOT NULL,
	wins    INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS connections_wins;
DROP TABLE IF EXISTS wordle_wins;
DROP TABLE IF EXISTS settings;
DROP INDEX IF EXISTS ledger_ref;
DROP INDEX IF EXISTS ledger_user;
DROP TABLE IF EXISTS ledger;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS commands;
