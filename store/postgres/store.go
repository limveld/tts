// Package postgres is the Postgres implementation of the bot's store. It is the
// production backend; store/sqlite stays the dev/test one, and store/storetest
// proves the two behave identically.
//
// Two things differ from the SQLite twin and both are deliberate:
//
//   - Every path that reads a balance in order to change it takes a row lock on
//     accounts first (see lockAccount). Postgres admits concurrent writers, so
//     the check-then-debit that SQLite gets for free from its single writer has
//     to be made explicit here. Credit is the exception: an append-only credit
//     cannot overdraw, and one landing after a Spend's snapshot only leaves the
//     payer richer than the check believed.
//
//   - Every ORDER BY over a display name carries COLLATE "C". A cluster initdb'd
//     in en_US.UTF-8 orders case- and punctuation-insensitively while SQLite
//     compares bytes; without this the two backends disagree on tie-breaks, and
//     it reads like a logic bug rather than a collation difference.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"tts/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a Postgres-backed store. The pool's defaults are fine for one bot
// process; there is deliberately no SetMaxOpenConns tuning here.
type Store struct {
	db *sql.DB
}

// Open connects to dsn (a postgres:// URL) and brings the schema up to date.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies every pending migration in migrations/. Like the SQLite twin
// it uses goose's provider API rather than the package-level dialect globals,
// because the migrate tool drives both dialects in one process.
func migrate(db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithLogger(quietLogger{}))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

// quietLogger drops goose's chatter; failures still come back as errors from Up.
type quietLogger struct{}

func (quietLogger) Printf(string, ...any) {}
func (quietLogger) Fatalf(string, ...any) {}

// Close closes the pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for table-level access the capability
// interfaces deliberately don't offer. It exists for cmd/store-migrate and
// nothing else: no code in bot/ or server/ may use it.
func (s *Store) DB() *sql.DB { return s.db }

// Get returns the command by name; ok is false if it doesn't exist.
func (s *Store) Get(name string) (store.Command, bool, error) {
	var c store.Command
	err := s.db.QueryRow(
		`SELECT name, response, cooldown, min_role, count FROM commands WHERE name = $1`, name,
	).Scan(&c.Name, &c.Response, &c.Cooldown, &c.MinRole, &c.Count)
	if err == sql.ErrNoRows {
		return store.Command{}, false, nil
	}
	if err != nil {
		return store.Command{}, false, err
	}
	return c, true, nil
}

// Add inserts a command. created is false (no error) if a command with that name
// already exists — the caller reports "already exists".
func (s *Store) Add(c store.Command) (created bool, err error) {
	if c.MinRole == "" {
		c.MinRole = "everyone"
	}
	res, err := s.db.Exec(
		`INSERT INTO commands (name, response, cooldown, min_role) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name) DO NOTHING`,
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
	res, err := s.db.Exec(`UPDATE commands SET response = $1 WHERE name = $2`, response, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Delete removes a command. found is false if it didn't exist.
func (s *Store) Delete(name string) (found bool, err error) {
	res, err := s.db.Exec(`DELETE FROM commands WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// List returns the command names, sorted. COLLATE "C" so the order matches
// SQLite's byte comparison.
func (s *Store) List() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM commands ORDER BY name COLLATE "C"`)
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
	_, err := s.db.Exec(`UPDATE commands SET count = count + 1 WHERE name = $1`, name)
	return err
}
