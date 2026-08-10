-- +goose Up

-- accounts is a pure lock token: a row per user_id and nothing else. There is
-- deliberately no balance column — the ledger stays the single source of truth,
-- so there is no derived value that can drift out of sync and no dual write to
-- get wrong. It exists because SUM(ledger) cannot be locked: no predicate lock
-- stops a concurrent INSERT, so a check-then-debit needs a concrete row to
-- serialize on.
--
-- It is separate from users rather than a column on it because a chat user can
-- reach Spend with no users row at all, and SELECT ... FOR UPDATE on a row that
-- does not exist takes no lock whatsoever.
--
-- SQLite creates it and never writes to it: one writer at a time makes the lock
-- unnecessary here. It exists so both backends have the same table list, which
-- is what lets the migrate tool stay a table-for-table copier with no special
-- cases.
CREATE TABLE IF NOT EXISTS accounts (
	user_id    TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0
);

-- game_rounds replaces the three *_round keys in settings. state is the game's
-- own JSON document and the store never looks inside it; room_id and ends_at are
-- lifted out purely so psql can answer "is a round pending, and when does it
-- end" without decoding a blob. The game code still reads both from its own
-- document, so nothing downstream depends on the split.
CREATE TABLE IF NOT EXISTS game_rounds (
	game       TEXT PRIMARY KEY,
	room_id    TEXT NOT NULL DEFAULT '',
	ends_at    INTEGER NOT NULL DEFAULT 0,  -- unix millis; 0 = no deadline
	state      TEXT NOT NULL,
	updated_at INTEGER NOT NULL DEFAULT 0   -- unix seconds
);

-- Carry the live rows over. The old clear path wrote '' rather than deleting the
-- key, so empty values are skipped: those are finished rounds, not in-flight
-- ones. All three documents already spell the deadline "endsAt" and the room
-- "roomID", so one expression serves all three.
INSERT OR IGNORE INTO game_rounds (game, room_id, ends_at, state, updated_at)
SELECT replace(key, '_round', ''),
       COALESCE(json_extract(value, '$.roomID'), ''),
       COALESCE(json_extract(value, '$.endsAt'), 0),
       value,
       0
FROM settings
WHERE key IN ('gamble_round', 'wordle_round', 'connections_round') AND value <> '';

DELETE FROM settings WHERE key IN ('gamble_round', 'wordle_round', 'connections_round');

-- +goose Down
DROP TABLE IF EXISTS game_rounds;
DROP TABLE IF EXISTS accounts;
