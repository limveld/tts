-- The Postgres twin of store/sqlite/migrations/00007_maze_wins.sql. Same table,
-- same columns, only the spelling differs — so the migrate tool can stay a
-- table-for-table copier.
--
-- Only the winner of a round is tallied here, not everyone who placed. Placement
-- rewards are paid in marks through the ledger, which is already the record of
-- who got what; a second table saying the same thing differently would be a
-- second thing to keep true.

-- +goose Up
CREATE TABLE IF NOT EXISTS maze_wins (
	user_id TEXT   PRIMARY KEY,
	login   TEXT   NOT NULL,
	display TEXT   NOT NULL,
	wins    BIGINT NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS maze_wins;
