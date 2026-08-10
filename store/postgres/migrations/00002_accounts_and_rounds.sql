-- The Postgres twin of store/sqlite/migrations/00002_accounts_and_rounds.sql.
--
-- Here accounts is not decoration: every check-then-debit takes SELECT ... FOR
-- UPDATE on the user's row first, which is the only thing serializing two
-- concurrent spends. It is a pure lock token — no balance column, so the ledger
-- stays the single source of truth and nothing derived can drift. And it is
-- separate from users because a chat user can reach Spend with no users row at
-- all, and FOR UPDATE on a row that does not exist takes no lock whatsoever.
--
-- The settings carry-over finds nothing on a real cutover: migrations run
-- against an empty database and the rows arrive afterwards, by copy. It is kept
-- anyway so the two dialects stay semantically identical — a deleted no-op is a
-- difference someone later has to reason about.

-- +goose Up
CREATE TABLE IF NOT EXISTS accounts (
	user_id    TEXT   PRIMARY KEY,
	created_at BIGINT NOT NULL DEFAULT 0,
	updated_at BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS game_rounds (
	game       TEXT   PRIMARY KEY,
	room_id    TEXT   NOT NULL DEFAULT '',
	ends_at    BIGINT NOT NULL DEFAULT 0,  -- unix millis; 0 = no deadline
	state      JSONB  NOT NULL,
	updated_at BIGINT NOT NULL DEFAULT 0   -- unix seconds
);

INSERT INTO game_rounds (game, room_id, ends_at, state, updated_at)
SELECT replace(key, '_round', ''),
       COALESCE(value::jsonb ->> 'roomID', ''),
       COALESCE((value::jsonb ->> 'endsAt')::bigint, 0),
       value::jsonb,
       0
FROM settings
WHERE key IN ('gamble_round', 'wordle_round', 'connections_round') AND value <> ''
ON CONFLICT (game) DO NOTHING;

DELETE FROM settings WHERE key IN ('gamble_round', 'wordle_round', 'connections_round');

-- +goose Down
DROP TABLE IF EXISTS game_rounds;
DROP TABLE IF EXISTS accounts;
