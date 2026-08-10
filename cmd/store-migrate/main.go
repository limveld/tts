// Command store-migrate copies the bot's database between backends and proves
// the copy is correct.
//
// It is bidirectional on purpose. SQLite → Postgres is the cutover; Postgres →
// SQLite is the rollback, and having it turns "revert to the archived bot.db and
// lose everything written since" into "copy back and lose nothing". That is the
// difference between a rollback plan and a rollback hope.
//
// Verification runs through the store's own Balance and Leaderboard rather than
// over raw SQL. Comparing SELECT SUM(delta) on both sides only proves the rows
// moved; comparing src.Balance(id) to dst.Balance(id) proves the application
// level invariant holds on both backends — which is the thing actually at risk,
// since the two run different SQL to compute it.
//
//	store-migrate -from bot.db -to "$TTS_DATABASE_URL"            # cutover
//	store-migrate -from "$TTS_DATABASE_URL" -to rollback.db       # come back
//	store-migrate -from bot.db -to "$TTS_DATABASE_URL" -verify-only
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"tts/store"
	"tts/store/postgres"
	"tts/store/sqlite"
)

// migrateStore is what the tool needs from a backend: the ledger reads it
// verifies through, the round reads it compares, and the raw handle for the
// table-level copy the capability interfaces deliberately don't offer.
type migrateStore interface {
	Balance(userID string) (int64, error)
	Leaderboard(n int) ([]store.LedgerEntry, error)
	LoadRound(game string) (store.Round, bool, error)
	DB() *sql.DB
	Close() error
}

// tables is the copy order. Nothing here has foreign keys, so the order is for
// readability rather than integrity — except that ledger carries explicit ids
// and therefore needs the sequence reset afterwards.
var tables = []struct {
	name    string
	columns []string
}{
	{"commands", []string{"name", "response", "cooldown", "min_role", "count"}},
	{"users", []string{"user_id", "login", "display", "last_seen"}},
	{"accounts", []string{"user_id", "created_at", "updated_at"}},
	{"ledger", []string{"id", "user_id", "delta", "reason", "ref", "ts"}},
	{"settings", []string{"key", "value"}},
	{"wordle_wins", []string{"user_id", "login", "display", "wins"}},
	{"connections_wins", []string{"user_id", "login", "display", "wins"}},
	{"game_rounds", []string{"game", "room_id", "ends_at", "state", "updated_at"}},
}

// games must match bot/rounds.go's constants. A round left behind at cutover is
// escrowed marks left behind with it.
var games = []string{"gamble", "wordle", "connections"}

// batchRows is how many rows go into one multi-VALUES insert. 17k rows is
// instant either way; this just keeps the parameter count under Postgres's
// 65535-per-statement limit with room to spare.
const batchRows = 500

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "store-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	fs := flag.NewFlagSet("store-migrate", flag.ContinueOnError)
	var (
		from        = fs.String("from", "", "source DSN (sqlite path or postgres:// URL)")
		to          = fs.String("to", "", "destination DSN")
		migrateOnly = fs.Bool("migrate-only", false, "run migrations on -to and exit (no copy)")
		verifyOnly  = fs.Bool("verify-only", false, "skip the copy, just compare -from and -to")
		force       = fs.Bool("force", false, "copy even though the destination already has rows")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("-to is required")
	}
	if *from == "" && !*migrateOnly {
		return fmt.Errorf("-from is required (unless -migrate-only)")
	}

	// Opening runs each backend's migrations, which is what lets the copier be a
	// dumb table-for-table loop: by the time it runs, the source already has
	// game_rounds populated by migration 00002 and the destination has somewhere
	// to put it.
	dst, dstBackend, err := open(*to)
	if err != nil {
		return fmt.Errorf("destination %s: %w", *to, err)
	}
	defer dst.Close()

	if *migrateOnly {
		v, err := schemaVersion(dst)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "store-migrate: %s migrated to schema %d\n", dstBackend, v)
		return nil
	}

	src, srcBackend, err := open(*from)
	if err != nil {
		return fmt.Errorf("source %s: %w", *from, err)
	}
	defer src.Close()

	srcVersion, err := schemaVersion(src)
	if err != nil {
		return err
	}
	dstVersion, err := schemaVersion(dst)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "store-migrate: %s %s (schema %d) -> %s %s (schema %d)\n",
		srcBackend, short(*from), srcVersion, dstBackend, short(*to), dstVersion)

	if !*verifyOnly {
		if err := refuseDirtyDestination(dst, *force); err != nil {
			return err
		}
		counts, err := copyAll(src, dst, dstBackend)
		if err != nil {
			return err
		}
		if dstBackend == store.PostgresBackend {
			next, err := resetLedgerSequence(dst)
			if err != nil {
				return err
			}
			counts["ledger_sequence"] = next
		}
		report(out, counts)
	}

	if err := verify(out, src, dst, dstBackend); err != nil {
		return err
	}
	if dstBackend == store.PostgresBackend {
		fmt.Fprintf(out, "OK — cut over:  TTS_DATABASE_URL='%s' mise run bot:service:install && mise run bot:service:restart\n", *to)
		fmt.Fprintf(out, "     rollback:  unset TTS_DATABASE_URL (%s is untouched), or copy back:\n", short(*from))
		fmt.Fprintf(out, "                store-migrate -from '%s' -to '%s' -force\n", *to, *from)
	} else {
		fmt.Fprintf(out, "OK — rolled back to %s. Unset TTS_DATABASE_URL and restart the bot:\n", short(*to))
		fmt.Fprintf(out, "     mise run bot:service:install && mise run bot:service:restart\n")
	}
	return nil
}

// open opens dsn through the same scheme dispatch the bot uses, so the tool and
// the bot can never disagree about what a DSN means.
func open(dsn string) (migrateStore, store.Backend, error) {
	backend, target := store.Classify(dsn)
	if backend == store.PostgresBackend {
		s, err := postgres.Open(target)
		if err != nil {
			return nil, backend, err
		}
		return s, backend, nil
	}
	s, err := sqlite.Open(target)
	if err != nil {
		return nil, backend, err
	}
	return s, backend, nil
}

func short(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		// Don't print credentials from a URL.
		rest := dsn[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return rest
	}
	return dsn
}

func schemaVersion(s migrateStore) (int64, error) {
	var v int64
	err := s.DB().QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&v)
	return v, err
}

// refuseDirtyDestination aborts unless the destination's data tables are empty.
// The cutover gets run by a tired human at some point; make the footgun require
// a flag.
func refuseDirtyDestination(dst migrateStore, force bool) error {
	for _, table := range []string{"commands", "users", "ledger"} {
		var n int64
		if err := dst.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return fmt.Errorf("counting %s: %w", table, err)
		}
		if n > 0 && !force {
			return fmt.Errorf("destination is not empty (%s has %d rows) — pass -force to copy anyway", table, n)
		}
	}
	return nil
}
