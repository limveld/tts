-- Torch Maze win tally. The third of these, and deliberately identical in shape
-- to the two 00001 created: one row per user who has escaped a maze, carrying
-- their current name so the leaderboard needs no join.
--
-- It is a new migration rather than an edit to the baseline because the baseline
-- is frozen. The live bot.db already matches 00001 statement for statement, and
-- that no-op property is the whole reason migration can be run against it
-- without a stamping trick. Folding a new table in there would break it.
--
-- The Postgres twin is store/postgres/migrations/00007_maze_wins.sql: same
-- table, same columns, so cmd/store-migrate stays a table-for-table copier.

-- +goose Up
CREATE TABLE IF NOT EXISTS maze_wins (
	user_id TEXT PRIMARY KEY,
	login   TEXT NOT NULL,
	display TEXT NOT NULL,
	wins    INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS maze_wins;
