-- The Postgres twin of store/sqlite/migrations/00008_maze_log.sql. Same tables,
-- same columns, same index — only the spelling differs, so the migrate tool can
-- stay a table-for-table copier. That file carries the reasoning; this one carries
-- the two differences that are real here.
--
-- NOT PARTITIONED, unlike chat_message in 00006. A round is a few hundred rows at
-- worst and this table is never pruned — partitioning exists to enable retention,
-- and permanence is the entire point of an archive. It would be the default
-- child, the premake window, the gopartman registration, the nightly agent and a
-- folded-counts seam in every query, for nothing.
--
-- input is JSONB here and TEXT on SQLite, which the copier already handles for
-- game_rounds.state — see cmd/store-migrate/copy.go, which has to cast on the way
-- in because the value arrives as text.

-- +goose Up

CREATE TABLE IF NOT EXISTS maze_rounds (
	id         TEXT   PRIMARY KEY,
	room_id    TEXT   NOT NULL,
	seed       BIGINT NOT NULL,
	started_at BIGINT NOT NULL,
	ended_at   BIGINT NOT NULL,
	tick_ms    BIGINT NOT NULL,
	cycles     BIGINT NOT NULL DEFAULT 0,
	-- An end-reason wire value, or 'skipped' for a moderator's !skipgame.
	reason     TEXT   NOT NULL DEFAULT '',
	players    BIGINT NOT NULL DEFAULT 0,
	finishers  BIGINT NOT NULL DEFAULT 0,
	winner_id      TEXT NOT NULL DEFAULT '',
	winner_login   TEXT NOT NULL DEFAULT '',
	winner_display TEXT NOT NULL DEFAULT '',
	input      JSONB  NOT NULL
);

CREATE TABLE IF NOT EXISTS maze_events (
	round_id TEXT   NOT NULL,
	seq      BIGINT NOT NULL,
	cycle    BIGINT NOT NULL,
	kind     TEXT   NOT NULL,
	seat     BIGINT NOT NULL DEFAULT -1,
	user_id  TEXT   NOT NULL DEFAULT '',
	login    TEXT   NOT NULL DEFAULT '',
	display  TEXT   NOT NULL DEFAULT '',
	at       TEXT   NOT NULL DEFAULT '',
	n        BIGINT NOT NULL DEFAULT 0,
	reason   TEXT   NOT NULL DEFAULT '',
	PRIMARY KEY (round_id, seq)
);

CREATE INDEX IF NOT EXISTS maze_events_kind ON maze_events (kind, round_id);

-- +goose Down
DROP INDEX IF EXISTS maze_events_kind;
DROP TABLE IF EXISTS maze_events;
DROP TABLE IF EXISTS maze_rounds;
