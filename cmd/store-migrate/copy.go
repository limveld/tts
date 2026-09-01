package main

import (
	"fmt"
	"strings"

	"tts/store"
)

// copyAll copies every table from src to dst inside one destination
// transaction, so a failure part-way leaves the destination as it was rather
// than half-populated.
func copyAll(src, dst migrateStore, dstBackend store.Backend) (map[string]int64, error) {
	counts := make(map[string]int64, len(tables))

	tx, err := dst.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, table := range tables {
		rows, err := src.DB().Query(
			`SELECT ` + strings.Join(table.columns, ", ") + ` FROM ` + table.name)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", table.name, err)
		}

		var batch [][]any
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			stmt, args := insertStatement(table.name, table.columns, len(batch), dstBackend)
			for _, row := range batch {
				args = append(args, row...)
			}
			if _, err := tx.Exec(stmt, args...); err != nil {
				return fmt.Errorf("writing %s: %w", table.name, err)
			}
			counts[table.name] += int64(len(batch))
			batch = batch[:0]
			return nil
		}

		for rows.Next() {
			vals := make([]any, len(table.columns))
			ptrs := make([]any, len(table.columns))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning %s: %w", table.name, err)
			}
			batch = append(batch, vals)
			if len(batch) >= batchRows {
				if err := flush(); err != nil {
					rows.Close()
					return nil, err
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading %s: %w", table.name, err)
		}
		rows.Close()
		if err := flush(); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return counts, nil
}

// insertStatement builds a multi-VALUES insert for n rows, in the destination's
// placeholder dialect.
//
// Two Postgres-only adjustments, both because the destination column is not
// quite the source column:
//
//   - the JSONB columns are cast: they arrive as text from SQLite, and JSONB
//     won't take them otherwise. See jsonbColumns.
//   - ledger and chat_message gain ts_at, their partition key, which exists only
//     on Postgres. It is derived rather than copied — the same to_timestamp(ts)
//     every INSERT in store/postgres writes — and it re-uses the ts placeholder
//     rather than binding another argument, so the caller's row-to-args loop
//     stays a straight copy of the source columns.
//
// Without that a copy into a partitioned table fails outright on the NOT NULL,
// which is the good outcome: the alternative, a DEFAULT, would put every copied
// row in the partition for the day of the copy.
// boolColumns are the columns Postgres declares BOOLEAN. SQLite has no boolean
// type and stores them as INTEGER 0/1, so a row read out of SQLite arrives here
// as int64 — which pgx refuses for a bool parameter ("cannot find encode plan"),
// and refuses at encode time, before any cast in the statement could help.
//
// ::int::boolean fixes it by changing what pgx is asked for rather than what it
// is given: Postgres infers the parameter as int4 from the first cast, the int64
// encodes cleanly, and the second cast lands it in the column. The reverse
// direction needs nothing — a Go bool is a valid SQLite parameter.
//
// A column set rather than a table/column pair because these four names are
// booleans wherever they appear, and a bare name that is a boolean in one table
// and an integer in another is a schema worth fixing rather than working around.
var boolColumns = map[string]bool{
	"is_mod": true, "is_sub": true, "is_vip": true, "is_broadcaster": true,
}

// jsonbColumns are the columns Postgres declares JSONB where SQLite stores TEXT.
// Keyed table.column rather than by bare name, unlike boolColumns: "state" and
// "input" are documents in these tables and could perfectly well be ordinary text
// somewhere else, so the pair is what is actually true.
//
// Missing an entry here is not subtle — the copy dies with "column is of type
// jsonb but expression is of type text" — but it dies at cutover, which is the
// worst time to find out.
var jsonbColumns = map[string]bool{
	"game_rounds.state": true,
	"maze_rounds.input": true,
}

func insertStatement(table string, columns []string, n int, backend store.Backend) (string, []any) {
	pg := backend == store.PostgresBackend
	derivedTsAt := pg && (table == "ledger" || table == "chat_message")

	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(columns, ", "))
	if derivedTsAt {
		b.WriteString(", ts_at")
	}
	b.WriteString(") VALUES ")

	arg := 1
	for row := 0; row < n; row++ {
		if row > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		tsArg := 0
		for i, col := range columns {
			if i > 0 {
				b.WriteString(", ")
			}
			if pg {
				fmt.Fprintf(&b, "$%d", arg)
				if jsonbColumns[table+"."+col] {
					b.WriteString("::jsonb")
				}
				if derivedTsAt && col == "ts" {
					b.WriteString("::bigint")
					tsArg = arg
				}
				if boolColumns[col] {
					b.WriteString("::int::boolean")
				}
				arg++
			} else {
				b.WriteByte('?')
			}
		}
		if derivedTsAt {
			// ::bigint on both uses of the placeholder, or Postgres tries to
			// deduce one type for a parameter used as both ts (bigint) and
			// to_timestamp's argument (double precision) and refuses.
			fmt.Fprintf(&b, ", to_timestamp($%d::bigint)", tsArg)
		}
		b.WriteByte(')')
	}
	return b.String(), make([]any, 0, n*len(columns))
}

// resetSequence moves a table's identity sequence past the highest copied id and
// returns the next value it will hand out.
//
// Miss this and the first live insert after cutover dies on a duplicate key —
// the single easiest thing to forget here and the most embarrassing to discover
// in production, because everything looks fine until someone earns a mark or
// says something.
//
// The table name is interpolated because pg_get_serial_sequence takes it as a
// value while MAX(id) needs it as an identifier; both callers pass a constant.
func resetSequence(dst migrateStore, table string) (int64, error) {
	var next int64
	err := dst.DB().QueryRow(fmt.Sprintf(
		`SELECT setval(pg_get_serial_sequence('%s', 'id'),
		               COALESCE((SELECT MAX(id) FROM %s), 0) + 1, false)`, table, table)).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("resetting %s sequence: %w", table, err)
	}
	return next, nil
}
