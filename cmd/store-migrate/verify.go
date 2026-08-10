package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"tts/store"
)

// verify compares source and destination and returns an error naming every
// mismatch. Nothing gets restarted until this passes.
func verify(out *os.File, src, dst migrateStore, dstBackend store.Backend) error {
	var problems []string
	note := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// Balances, through the public API on both sides. This is the check that
	// matters: the two backends run different SQL to compute a balance, so
	// comparing SUM(delta) on each would only prove the rows moved.
	ids, err := ledgerUserIDs(src)
	if err != nil {
		return err
	}
	matched, mismatched := 0, 0
	for _, id := range ids {
		a, err := src.Balance(id)
		if err != nil {
			return fmt.Errorf("source balance %s: %w", id, err)
		}
		b, err := dst.Balance(id)
		if err != nil {
			return fmt.Errorf("destination balance %s: %w", id, err)
		}
		if a == b {
			matched++
			continue
		}
		mismatched++
		if mismatched <= 20 {
			note("balance %s: source %d, destination %d", id, a, b)
		}
	}
	if mismatched > 20 {
		note("... and %d more balance mismatches", mismatched-20)
	}

	// Row counts, per table.
	countsMatch := true
	for _, table := range tables {
		a, err := rowCount(src, table.name)
		if err != nil {
			return err
		}
		b, err := rowCount(dst, table.name)
		if err != nil {
			return err
		}
		if a != b {
			countsMatch = false
			note("%s row count: source %d, destination %d", table.name, a, b)
		}
	}

	// Ledger totals: catches a copy that moved the right number of rows with the
	// wrong values in them.
	srcSum, srcMax, err := ledgerTotals(src)
	if err != nil {
		return err
	}
	dstSum, dstMax, err := ledgerTotals(dst)
	if err != nil {
		return err
	}
	if srcSum != dstSum {
		note("sum(delta): source %d, destination %d", srcSum, dstSum)
	}
	if srcMax != dstMax {
		note("max(ledger.id): source %d, destination %d", srcMax, dstMax)
	}

	// The leaderboard, element-wise. This is what catches the collation trap
	// before production does: identical rows can still come back in a different
	// order if the destination's ORDER BY is locale-aware.
	lbMatch, err := compareLeaderboards(src, dst, note)
	if err != nil {
		return err
	}

	// Rounds. A round left behind is escrowed marks left behind.
	if err := compareRounds(src, dst, note); err != nil {
		return err
	}

	// The destination sequence must be past max(id), or the first live Credit
	// after cutover dies on a duplicate key.
	if dstBackend == store.PostgresBackend {
		next, err := ledgerSequenceNext(dst)
		if err != nil {
			return err
		}
		if next <= dstMax {
			note("ledger sequence is at %d but max(id) is %d — the next Credit would collide", next, dstMax)
		}
	}

	fmt.Fprintf(out, "verify: %d/%d balances match  ·  sum(delta) %s == %s\n",
		matched, len(ids), comma(srcSum), comma(dstSum))
	fmt.Fprintf(out, "        counts %s  ·  leaderboard top-50 %s\n",
		okWord(countsMatch), okWord(lbMatch))

	if len(problems) > 0 {
		return fmt.Errorf("verification FAILED:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func okWord(ok bool) string {
	if ok {
		return "match"
	}
	return "MISMATCH"
}

// ledgerUserIDs is the union of user_id in users and in ledger — a user can have
// marks with no identity row, and an identity row with no marks, and both must
// come across.
func ledgerUserIDs(s migrateStore) ([]string, error) {
	rows, err := s.DB().Query(
		`SELECT user_id FROM users UNION SELECT user_id FROM ledger`)
	if err != nil {
		return nil, fmt.Errorf("listing user ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, rows.Err()
}

func rowCount(s migrateStore, table string) (int64, error) {
	var n int64
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", table, err)
	}
	return n, nil
}

func ledgerTotals(s migrateStore) (sum, max int64, err error) {
	err = s.DB().QueryRow(
		`SELECT COALESCE(SUM(delta), 0), COALESCE(MAX(id), 0) FROM ledger`).Scan(&sum, &max)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger totals: %w", err)
	}
	return sum, max, nil
}

// ledgerSequenceNext returns the id the next insert will be given. setval(…,
// n, false) leaves is_called false so the next value is n itself; after ordinary
// inserts is_called is true and the next value is last_value+1.
func ledgerSequenceNext(s migrateStore) (int64, error) {
	var seq string
	if err := s.DB().QueryRow(`SELECT pg_get_serial_sequence('ledger', 'id')`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("locating ledger sequence: %w", err)
	}
	var next int64
	if err := s.DB().QueryRow(
		`SELECT last_value + CASE WHEN is_called THEN 1 ELSE 0 END FROM ` + seq).Scan(&next); err != nil {
		return 0, fmt.Errorf("reading ledger sequence %s: %w", seq, err)
	}
	return next, nil
}

func compareLeaderboards(src, dst migrateStore, note func(string, ...any)) (bool, error) {
	a, err := src.Leaderboard(50)
	if err != nil {
		return false, fmt.Errorf("source leaderboard: %w", err)
	}
	b, err := dst.Leaderboard(50)
	if err != nil {
		return false, fmt.Errorf("destination leaderboard: %w", err)
	}
	if len(a) != len(b) {
		note("leaderboard length: source %d, destination %d", len(a), len(b))
		return false, nil
	}
	ok := true
	for i := range a {
		if a[i] != b[i] {
			note("leaderboard[%d]: source %+v, destination %+v", i, a[i], b[i])
			ok = false
		}
	}
	// Same rows in a different order is the collation trap specifically, and it
	// is worth saying so: the values all check out, so nothing else here points
	// at the ORDER BY.
	if !ok && sameSet(a, b) {
		note("leaderboard has the same rows in a different order — check COLLATE \"C\" on the ORDER BY")
	}
	return ok, nil
}

func sameSet(a, b []store.LedgerEntry) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[store.LedgerEntry]int, len(a))
	for _, e := range a {
		seen[e]++
	}
	for _, e := range b {
		seen[e]--
		if seen[e] < 0 {
			return false
		}
	}
	return true
}

func compareRounds(src, dst migrateStore, note func(string, ...any)) error {
	for _, game := range games {
		a, aOK, err := src.LoadRound(game)
		if err != nil {
			return fmt.Errorf("source round %s: %w", game, err)
		}
		b, bOK, err := dst.LoadRound(game)
		if err != nil {
			return fmt.Errorf("destination round %s: %w", game, err)
		}
		if aOK != bOK {
			note("round %s: source present=%v, destination present=%v", game, aOK, bOK)
			continue
		}
		if !aOK {
			continue
		}
		if a.RoomID != b.RoomID || a.EndsAt != b.EndsAt {
			note("round %s columns: source %s/%d, destination %s/%d", game, a.RoomID, a.EndsAt, b.RoomID, b.EndsAt)
		}
		// Semantically, never as bytes: JSONB normalizes key order and
		// whitespace, so a byte compare would fail on an identical document.
		same, err := sameJSON(a.State, b.State)
		if err != nil {
			return fmt.Errorf("round %s state: %w", game, err)
		}
		if !same {
			note("round %s state differs", game)
		}
	}
	return nil
}

func sameJSON(a, b []byte) (bool, error) {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

// comma renders n with thousands separators, so 16864 reads as 16,864 in the
// summary a human is checking against a count they wrote down.
func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// report prints the per-table counts, aligned, so they can be checked against
// numbers written down before the cutover.
func report(out *os.File, counts map[string]int64) {
	for _, table := range tables {
		n := counts[table.name]
		extra := ""
		if table.name == "accounts" && n == 0 {
			extra = "   (created on demand)"
		}
		if table.name == "ledger" {
			if next, ok := counts["ledger_sequence"]; ok {
				extra = fmt.Sprintf("   sequence -> %s", comma(next))
			}
		}
		fmt.Fprintf(out, "  %-18s %10s%s\n", table.name, comma(n), extra)
	}
}
