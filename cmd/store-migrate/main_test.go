package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tts/store"
	"tts/store/sqlite"
	"tts/store/storetest"
)

// runTool invokes the tool the way a human would and returns its output plus
// whether it succeeded. Nothing here shells out — the point is to exercise the
// same run() the binary does.
func runTool(t *testing.T, args ...string) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runErr := run(args, f)
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b), runErr
}

// seedSource fills a temp SQLite store through the public API — every table,
// including the awkward ones: a ledger row with a ref (the partial index), a
// round document (JSON that has to survive a JSONB round trip), and both
// tallies.
func seedSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Add(store.Command{Name: "discord", Response: "join $user", Cooldown: 5}); err != nil {
		t.Fatal(err)
	}
	// A logged maze round, so the replay tables are actually exercised by the copy.
	// A table that is empty on both sides "matches", so an unseeded table is an
	// uncopied table nobody would notice.
	if err := s.MazeLogRound(store.MazeRound{
		ID: "1700000000000-1092", RoomID: "12345", Seed: 4242,
		StartedAt: 1_700_000_000, EndedAt: 1_700_000_200, TickMS: 10000,
		Cycles: 18, Reason: "placements-closed", Players: 5, Finishers: 4,
		WinnerID: "u1", WinnerLogin: "bob", WinnerDisplay: "Bob",
		Input: []byte(`{"board":{"size":6},"moves":[{"cycle":1,"seat":0,"dir":"up"}]}`),
	}, []store.MazeEvent{
		{RoundID: "1700000000000-1092", Seq: 0, Cycle: 0, Kind: "seats-locked", Seat: -1, N: 4},
		{RoundID: "1700000000000-1092", Seq: 1, Cycle: 3, Kind: "key-taken", Seat: 0,
			UserID: "u1", Login: "bob", Display: "Bob", At: "C4", N: 3},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Add(store.Command{Name: "socials", Response: "links"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ id, login, display string }{
		{"u1", "bob", "Bob"}, {"u2", "amy", "Amy"}, {"u3", "zed", "Zed"},
	} {
		if err := s.UpsertUser(u.id, u.login, u.display); err != nil {
			t.Fatal(err)
		}
	}
	s.Credit("u1", 500, "accrual", "")
	s.Credit("u1", 250, "convert", "redemption-abc") // exercises the partial unique index
	s.Credit("u2", 900, "accrual", "")
	s.Spend("u1", 100, "tts")
	s.Transfer("u2", "u3", 50, "give")
	// A user with marks but no identity row, and one with an identity row but no
	// marks: the verifier's id union has to cover both.
	s.Credit("nameless", 42, "accrual", "")

	if err := s.SetSetting("charge_mode", "paid"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("depth_points", "1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WordleAddWin("u1", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConnectionsAddWin("u2", "amy", "Amy"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRound("gamble", "room1", 1723312860000,
		[]byte(`{"id":"r1","roomID":"room1","buyIn":100,"endsAt":1723312860000,`+
			`"entrants":[{"userID":"u1","login":"bob","display":"Bob"}],"winner":-1}`)); err != nil {
		t.Fatal(err)
	}

	// Chat lines, including a tombstoned one: the destination's chat_message is
	// partitioned and derives ts_at from ts, so a copy that got that wrong would
	// either fail on the NOT NULL or file every line under the day of the copy.
	if err := s.LogMessages([]store.ChatMessage{
		{TS: 1723312800, RoomID: "room1", MsgID: "c1", UserID: "u1", Login: "bob", Display: "Bob", Text: "hello", IsMod: true},
		{TS: 1723312860, RoomID: "room1", MsgID: "c2", UserID: "u2", Login: "amy", Display: "Amy", Text: "hi", Emotes: "25:0-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDeleted("room1", "c2", 1723312900); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRoundTripToPostgres(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	out, err := runTool(t, "-from", src, "-to", dsn)
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	for _, want := range []string{"balances match", "counts match", "leaderboard top-50 match"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// A standalone verify pass over the copy the first run just made.
	if out, err := runTool(t, "-from", src, "-to", dsn, "-verify-only"); err != nil {
		t.Fatalf("verify-only: %v\n%s", err, out)
	}
}

// After a copy, a live Credit must get MAX(id)+1. Without the sequence reset the
// first mark anyone earns after cutover dies on a duplicate key — and everything
// looks fine until that moment.
func TestSequenceIsPastMaxID(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	var maxBefore int64
	if err := dst.DB().QueryRow(`SELECT MAX(id) FROM ledger`).Scan(&maxBefore); err != nil {
		t.Fatal(err)
	}
	var got int64
	if err := dst.DB().QueryRow(
		`INSERT INTO ledger (user_id, delta, reason, ts, ts_at) VALUES ('after', 1, 'accrual', 0, to_timestamp(0)) RETURNING id`,
	).Scan(&got); err != nil {
		t.Fatalf("first credit after copy failed — sequence not reset: %v", err)
	}
	if got != maxBefore+1 {
		t.Errorf("new ledger id=%d want %d (max+1)", got, maxBefore+1)
	}
}

// chat_message is the second partitioned table, so the copy has to derive its
// ts_at the same way the ledger's is derived. Getting it wrong has two failure
// modes and this catches both: a missing ts_at trips the NOT NULL, and a
// defaulted one files every copied line under the day of the copy, where the
// history it claims to be is unreachable.
func TestChatMessagesSurviveTheCopy(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	var total, mismatched int
	if err := dst.DB().QueryRow(
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE ts_at <> to_timestamp(ts)) FROM chat_message`,
	).Scan(&total, &mismatched); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("copied %d chat rows want 2", total)
	}
	if mismatched != 0 {
		t.Errorf("%d of %d copied rows have ts_at out of step with ts", mismatched, total)
	}

	// Every column, including the tombstone and the role bits — a copier that
	// dropped one would still pass a row count.
	var text, emotes, deletedBy string
	var isMod bool
	var deletedAt int64
	if err := dst.DB().QueryRow(
		`SELECT text, emotes, is_mod, deleted_at, deleted_by FROM chat_message WHERE msg_id = 'c2'`,
	).Scan(&text, &emotes, &isMod, &deletedAt, &deletedBy); err != nil {
		t.Fatal(err)
	}
	if text != "hi" || emotes != "25:0-1" || isMod {
		t.Errorf("c2 = text %q emotes %q is_mod %v", text, emotes, isMod)
	}
	if deletedAt != 1723312900 || deletedBy != "clearmsg" {
		t.Errorf("c2 tombstone = %d/%q, want it carried across the copy", deletedAt, deletedBy)
	}

	// And the sequence, or the first line spoken after cutover collides.
	var maxBefore, got int64
	if err := dst.DB().QueryRow(`SELECT MAX(id) FROM chat_message`).Scan(&maxBefore); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB().QueryRow(
		`INSERT INTO chat_message (ts, ts_at, room_id, msg_id, user_id, login, display, text)
		 VALUES (0, to_timestamp(0), 'room1', 'after', 'u1', 'bob', 'Bob', 'hi') RETURNING id`,
	).Scan(&got); err != nil {
		t.Fatalf("first chat line after copy failed — sequence not reset: %v", err)
	}
	if got != maxBefore+1 {
		t.Errorf("new chat_message id=%d want %d (max+1)", got, maxBefore+1)
	}
}

// The verifier has to be able to fail, or it is decoration. Since balance is
// materialized there are two independent ways a copy can be wrong, and they are
// caught by two different checks — so both get a case.

// Corrupting the money itself: accounts.balance is what everyone's marks read
// from, so this is the one that would be visible in chat.
func TestVerifyCatchesMutatedBalance(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.DB().Exec(
		`UPDATE accounts SET balance = balance - 7 WHERE user_id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	out, err := runTool(t, "-from", src, "-to", dsn, "-verify-only")
	if err == nil {
		t.Fatalf("verify passed after a balance was mutated:\n%s", out)
	}
	if !strings.Contains(err.Error(), "balance u1") {
		t.Errorf("error should name the offending user; got: %v", err)
	}
}

// Corrupting the history instead. Every balance still agrees across the copy —
// accounts.balance was untouched — so the per-user comparison passes and only
// the internal-consistency check notices. Before balance was materialized this
// was the same failure as the case above; it is now a genuinely different one,
// and the check that catches it did not previously exist.
func TestVerifyCatchesLedgerDriftingFromBalance(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.DB().Exec(
		`INSERT INTO ledger (user_id, delta, reason, ts, ts_at) VALUES ('u1', -7, 'sabotage', 0, to_timestamp(0))`); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	out, err := runTool(t, "-from", src, "-to", dsn, "-verify-only")
	if err == nil {
		t.Fatalf("verify passed with a ledger row that no balance reflects:\n%s", out)
	}
	if !strings.Contains(err.Error(), "the ledger sums to") {
		t.Errorf("error should report the balance/ledger inconsistency; got: %v", err)
	}
}

func TestVerifyCatchesMissingRow(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.DB().Exec(`DELETE FROM commands WHERE name = 'socials'`); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	out, err := runTool(t, "-from", src, "-to", dsn, "-verify-only")
	if err == nil {
		t.Fatalf("verify passed with a command row deleted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "commands row count") {
		t.Errorf("error should name the count check; got: %v", err)
	}
}

func TestRefusesDirtyDestination(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("first copy: %v\n%s", err, out)
	}

	_, err := runTool(t, "-from", src, "-to", dsn)
	if err == nil {
		t.Fatal("second copy without -force was allowed")
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Errorf("refusal should point at -force; got: %v", err)
	}

	// With -force it proceeds. The rows are duplicated, so verification is
	// expected to fail — what's being tested is that the guard was bypassed, not
	// that the result is sane.
	out, forceErr := runTool(t, "-from", src, "-to", dsn, "-force")
	if strings.Contains(out, "destination is not empty") {
		t.Error("-force did not bypass the guard")
	}
	if forceErr == nil {
		t.Error("copying over existing rows should fail verification")
	}
}

// Postgres -> a fresh SQLite file: the rollback direction. Losing this is what
// turns a rollback plan back into a rollback hope.
func TestReverseDirection(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	src := seedSource(t)

	if out, err := runTool(t, "-from", src, "-to", dsn); err != nil {
		t.Fatalf("forward copy: %v\n%s", err, out)
	}

	back := filepath.Join(t.TempDir(), "back.db")
	out, err := runTool(t, "-from", dsn, "-to", back)
	if err != nil {
		t.Fatalf("reverse copy: %v\n%s", err, out)
	}

	// The round document survived the SQLite -> JSONB -> SQLite round trip, and
	// the ledger's ref idempotency came with it.
	s, err := sqlite.Open(back)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	round, ok, err := s.LoadRound("gamble")
	if err != nil || !ok {
		t.Fatalf("round lost in the round trip: ok=%v err=%v", ok, err)
	}
	if round.RoomID != "room1" || round.EndsAt != 1723312860000 {
		t.Errorf("round columns=%s/%d want room1/1723312860000", round.RoomID, round.EndsAt)
	}
	if credited, err := s.Credit("u1", 250, "convert", "redemption-abc"); err != nil || credited {
		t.Errorf("ref idempotency lost: credited=%v err=%v want false/nil", credited, err)
	}
}

// -migrate-only must create the schema without needing (or touching) a source.
func TestMigrateOnly(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))

	out, err := runTool(t, "-to", dsn, "-migrate-only")
	if err != nil {
		t.Fatalf("migrate-only: %v\n%s", err, out)
	}
	dst, _, err := open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	// Compared against what goose actually applied rather than a literal, so
	// adding a migration doesn't fail a test that has nothing to do with it.
	var applied int64
	if err := dst.DB().QueryRow(
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = true`).Scan(&applied); err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	if want := fmt.Sprintf("schema %d", applied); !strings.Contains(out, want) {
		t.Errorf("output should report %q:\n%s", want, out)
	}

	var n int64
	if err := dst.DB().QueryRow(`SELECT COUNT(*) FROM ledger`).Scan(&n); err != nil {
		t.Fatalf("ledger table missing after -migrate-only: %v", err)
	}
	if n != 0 {
		t.Errorf("ledger has %d rows after -migrate-only, want 0", n)
	}
}

// SQLite -> SQLite needs no Postgres, so this one always runs. It covers the
// copier itself: batching, the column lists, and the verifier's happy path.
func TestSQLiteToSQLite(t *testing.T) {
	src := seedSource(t)
	dst := filepath.Join(t.TempDir(), "dst.db")

	out, err := runTool(t, "-from", src, "-to", dst)
	if err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "balances match") {
		t.Errorf("output:\n%s", out)
	}
}

// Batching has to hold across the 500-row boundary, and 17k rows is the real
// shape of the cutover.
func TestCopyAcrossBatchBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUser("u1", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	const rows = batchRows*2 + 7
	for i := 0; i < rows; i++ {
		if _, err := s.Credit("u1", 1, "accrual", ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	dst := filepath.Join(t.TempDir(), "many-dst.db")
	if out, err := runTool(t, "-from", path, "-to", dst); err != nil {
		t.Fatalf("copy: %v\n%s", err, out)
	}

	d, err := sqlite.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if b, _ := d.Balance("u1"); b != rows {
		t.Errorf("balance=%d want %d — a batch was dropped", b, rows)
	}
}
