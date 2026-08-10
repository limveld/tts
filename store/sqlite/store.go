// Package sqlite is the SQLite implementation of the bot's store
// (modernc.org/sqlite — pure Go, no CGo). It is the default backend: no daemon,
// one file, and the fast hermetic choice for tests. The domain types it reads
// and writes live in the parent store package.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"tts/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a SQLite-backed store: one file holding custom commands, the marks
// ledger, settings and the game tallies.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and brings its
// schema up to date. A fresh file comes up fully migrated; an existing one is
// carried forward. Migrations run here rather than in a separate command because
// the bot fails fast on a store error — there is no half-migrated state it could
// limp along in.
func Open(path string) (*Store, error) {
	// _txlock=immediate makes every database/sql transaction take the write lock
	// up front. The default is a DEFERRED transaction, where the read takes a
	// shared lock and the first write attempts an upgrade — and a failed upgrade
	// returns SQLITE_BUSY immediately rather than waiting out busy_timeout. The
	// read-then-write money paths (Grant/Spend/Transfer) depend on this.
	//
	// busy_timeout is set via _pragma so it applies to every pooled connection;
	// running it as a one-off Exec only configured whichever connection served it.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// WAL is a property of the file, not the connection, so one Exec is enough.
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies every pending migration in migrations/.
//
// It uses goose's provider API rather than the package-level goose.SetDialect /
// goose.Up globals: the migrate tool and the conformance suite both drive the
// SQLite and Postgres dialects in one process, and a process-global dialect is a
// footgun there.
func migrate(db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	// goose logs to stdout by default, which under launchd lands unlabelled in
	// tts-bot.out.log. Silence it; Open's caller owns the logging.
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub, goose.WithLogger(quietLogger{}))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	return nil
}

// quietLogger drops goose's chatter. Migration *failures* come back as errors
// from Up, so nothing diagnostic is lost.
type quietLogger struct{}

func (quietLogger) Printf(string, ...any) {}
func (quietLogger) Fatalf(string, ...any) {}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Get returns the command by name; ok is false if it doesn't exist.
func (s *Store) Get(name string) (store.Command, bool, error) {
	var c store.Command
	err := s.db.QueryRow(
		`SELECT name, response, cooldown, min_role, count FROM commands WHERE name = ?`, name,
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
