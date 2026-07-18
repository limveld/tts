// Package store persists the bot's chat-managed data in a local SQLite database
// (modernc.org/sqlite — pure Go, no CGo). Stage 2 holds custom commands; later
// stages (loyalty points) add tables to the same DB.
package store

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Command is one chat-managed custom command. Name is without the leading "!"
// and lowercased. Cooldown is a global per-command cooldown in seconds; MinRole
// is everyone|sub|vip|mod.
type Command struct {
	Name     string
	Response string
	Cooldown int
	MinRole  string
	Count    int
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One bot process writes; a busy timeout + WAL avoid transient "database is
	// locked" between the command handlers and (later) the points loop.
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS commands (
			name     TEXT PRIMARY KEY,
			response TEXT NOT NULL,
			cooldown INTEGER NOT NULL DEFAULT 0,
			min_role TEXT NOT NULL DEFAULT 'everyone',
			count    INTEGER NOT NULL DEFAULT 0
		)`,
		// Stage 3 loyalty-points ("marks") economy: an identity table and an
		// append-only ledger (balance = SUM(delta)). See store/points.go.
		`CREATE TABLE IF NOT EXISTS users (
			user_id   TEXT PRIMARY KEY,
			login     TEXT NOT NULL,
			display   TEXT NOT NULL,
			last_seen INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ledger (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			delta   INTEGER NOT NULL,
			reason  TEXT NOT NULL,
			ref     TEXT,
			ts      INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS ledger_user ON ledger(user_id)`,
		// Idempotent channel-point crediting: a redemption id credits at most once.
		`CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL`,
		// Small key/value store for runtime toggles (e.g. the free/paid charge mode,
		// the depth points total, and the current Wordle round JSON).
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// Wordle win tally (one row per solver). See store/wordle.go.
		`CREATE TABLE IF NOT EXISTS wordle_wins (
			user_id TEXT PRIMARY KEY,
			login   TEXT NOT NULL,
			display TEXT NOT NULL,
			wins    INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Get returns the command by name; ok is false if it doesn't exist.
func (s *Store) Get(name string) (Command, bool, error) {
	var c Command
	err := s.db.QueryRow(
		`SELECT name, response, cooldown, min_role, count FROM commands WHERE name = ?`, name,
	).Scan(&c.Name, &c.Response, &c.Cooldown, &c.MinRole, &c.Count)
	if err == sql.ErrNoRows {
		return Command{}, false, nil
	}
	if err != nil {
		return Command{}, false, err
	}
	return c, true, nil
}

// Add inserts a command. created is false (no error) if a command with that name
// already exists — the caller reports "already exists".
func (s *Store) Add(c Command) (created bool, err error) {
	if c.MinRole == "" {
		c.MinRole = "everyone"
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO commands (name, response, cooldown, min_role) VALUES (?, ?, ?, ?)`,
		c.Name, c.Response, c.Cooldown, c.MinRole)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetResponse updates a command's response (for !editcom). found is false if the
// command doesn't exist.
func (s *Store) SetResponse(name, response string) (found bool, err error) {
	res, err := s.db.Exec(`UPDATE commands SET response = ? WHERE name = ?`, response, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Delete removes a command. found is false if it didn't exist.
func (s *Store) Delete(name string) (found bool, err error) {
	res, err := s.db.Exec(`DELETE FROM commands WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// List returns the command names, sorted.
func (s *Store) List() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM commands ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// IncCount increments a command's use counter (for $count).
func (s *Store) IncCount(name string) error {
	_, err := s.db.Exec(`UPDATE commands SET count = count + 1 WHERE name = ?`, name)
	return err
}
