-- The SQLite twin of store/postgres/migrations/00006_chat_log.sql.
--
-- All three tables are real here, because six conformance-tested methods touch
-- them and store/storetest would otherwise be testing two different programs.
-- What is absent is the partitioning: chat_message is one ordinary table and
-- never gains ts_at, exactly as 00005 left the ledger. chat_folded is created
-- and never written, following the precedent accounts set in 00002 and
-- ledger_folded set in 00004 — there are no partitions here to fold.
--
-- chat_stats therefore stays empty on SQLite, and that is correct rather than
-- broken: nothing is ever dropped, so every message is still in chat_message and
-- the live COUNT is the whole total. See docs/adr/0003-chat-log.md.

-- +goose Up

CREATE TABLE IF NOT EXISTS chat_message (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      INTEGER NOT NULL,
	room_id TEXT    NOT NULL,
	-- Twitch's own message id (the `id` tag) — what CLEARMSG targets. No UNIQUE
	-- constraint, matching the Postgres twin, which cannot have one on a
	-- partitioned table. Adding one here would make the two backends disagree
	-- about what is an error.
	msg_id  TEXT    NOT NULL,
	user_id TEXT    NOT NULL,
	-- Denormalized rather than joined from users, which is written only by the
	-- economy and is empty when the economy is off. wordle_wins and
	-- connections_wins set this precedent.
	login   TEXT    NOT NULL,
	display TEXT    NOT NULL,
	-- Raw, exactly as it arrived: the stripped form is derivable from
	-- text + emotes, the original is not derivable from the stripped one.
	text    TEXT    NOT NULL,
	emotes  TEXT    NOT NULL DEFAULT '',

	is_mod         BOOLEAN NOT NULL DEFAULT 0,
	is_sub         BOOLEAN NOT NULL DEFAULT 0,
	is_vip         BOOLEAN NOT NULL DEFAULT 0,
	is_broadcaster BOOLEAN NOT NULL DEFAULT 0,

	-- 0 = live, per the "0 = none" convention already used by game_rounds.ends_at
	-- and ledger_opening.through_ts. Text is deliberately kept on a tombstone;
	-- erasure is `chat-partition -purge-user`, which is a real delete.
	deleted_at INTEGER NOT NULL DEFAULT 0,
	deleted_by TEXT    NOT NULL DEFAULT ''   -- 'clearmsg' | 'clearchat' | ''
);

CREATE INDEX IF NOT EXISTS chat_message_user ON chat_message (user_id, ts DESC);
CREATE INDEX IF NOT EXISTS chat_message_msg  ON chat_message (msg_id);

-- The fold target. Real here so the conformance suite can read it on both
-- backends, but never written on SQLite: cmd/chat-partition refuses a non-
-- Postgres DSN outright, the way cmd/pg-partition already does.
CREATE TABLE IF NOT EXISTS chat_stats (
	user_id  TEXT    PRIMARY KEY,
	login    TEXT    NOT NULL,
	display  TEXT    NOT NULL,
	messages INTEGER NOT NULL DEFAULT 0,
	chars    INTEGER NOT NULL DEFAULT 0,
	first_ts INTEGER NOT NULL DEFAULT 0,
	last_ts  INTEGER NOT NULL DEFAULT 0
);

-- Created, never written: there are no partitions here to fold.
CREATE TABLE IF NOT EXISTS chat_folded (
	name       TEXT    PRIMARY KEY,
	from_ts    INTEGER NOT NULL,
	through_ts INTEGER NOT NULL,
	rows       INTEGER NOT NULL,
	folded_at  INTEGER NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS chat_folded;
DROP TABLE IF EXISTS chat_stats;
DROP TABLE IF EXISTS chat_message;
