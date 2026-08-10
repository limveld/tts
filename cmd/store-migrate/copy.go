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
// placeholder dialect. game_rounds.state is cast to jsonb on a Postgres
// destination: it arrives as text from SQLite, and JSONB won't take it
// otherwise.
func insertStatement(table string, columns []string, n int, backend store.Backend) (string, []any) {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(columns, ", "))
	b.WriteString(") VALUES ")

	arg := 1
	for row := 0; row < n; row++ {
		if row > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for i, col := range columns {
			if i > 0 {
				b.WriteString(", ")
			}
			if backend == store.PostgresBackend {
				fmt.Fprintf(&b, "$%d", arg)
				if table == "game_rounds" && col == "state" {
					b.WriteString("::jsonb")
				}
				arg++
			} else {
				b.WriteByte('?')
			}
		}
		b.WriteByte(')')
	}
	return b.String(), make([]any, 0, n*len(columns))
}

// resetLedgerSequence moves the identity sequence past the highest copied id and
// returns the next value it will hand out.
//
// Miss this and the first live Credit after cutover dies on a duplicate key —
// the single easiest thing to forget here and the most embarrassing to discover
// in production, because everything looks fine until someone earns a mark.
func resetLedgerSequence(dst migrateStore) (int64, error) {
	var next int64
	err := dst.DB().QueryRow(
		`SELECT setval(pg_get_serial_sequence('ledger', 'id'),
		               COALESCE((SELECT MAX(id) FROM ledger), 0) + 1, false)`).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("resetting ledger sequence: %w", err)
	}
	return next, nil
}
