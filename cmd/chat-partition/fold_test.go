package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tts/internal/partition"
	"tts/store/postgres"
	"tts/store/storetest"
)

// harness builds an isolated schema with the real migrations applied and hands
// back everything a case needs.
//
// The partman metadata cleanup is not optional. partman is a hardcoded global
// schema, so its parent_tables rows outlive the temp schema that DROP SCHEMA
// CASCADE takes with it — and every later Maintain in the whole package would
// then try to provision into schemas that no longer exist.
func harness(t *testing.T) (Config, *pgxpool.Pool) {
	t.Helper()
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))

	s, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	schema := dsn[strings.LastIndex(dsn, "search_path=")+len("search_path="):]

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM partman.parent_tables WHERE schema_name = $1`, schema)
		pool.Close()
	})

	return Config{
		DSN:       dsn,
		Schema:    schema,
		Premake:   defaultPremake,
		Retention: defaultRetention,
	}, pool
}

// seedAcrossDays writes chat lines dated into the past. The bot always stamps
// now(), so back-dated history has to be inserted directly — and it has to go
// into partitions that exist, which is what makeDay is for.
//
// Two lines per user per day, so a fold's COUNT is distinguishable from its
// chatter count.
func seedAcrossDays(t *testing.T, pool *pgxpool.Pool, cfg Config, days, users int) {
	t.Helper()
	ctx := context.Background()
	for d := days; d >= 1; d-- {
		day := time.Now().UTC().AddDate(0, 0, -d).Truncate(24 * time.Hour)
		makeDay(t, pool, cfg, day)
		for u := range users {
			for n := range 2 {
				ts := day.Add(12 * time.Hour).Add(time.Duration(n) * time.Minute).Unix()
				if _, err := pool.Exec(ctx, fmt.Sprintf(`
					INSERT INTO %s (ts, ts_at, room_id, msg_id, user_id, login, display, text)
					VALUES ($1::bigint, to_timestamp($1::bigint), 'room1', $2, $3, $4, $5, 'hello')`,
					partition.QuoteQualified(cfg.Schema, "chat_message")),
					ts,
					fmt.Sprintf("m-%d-%d-%d", d, u, n),
					fmt.Sprintf("u%d", u),
					fmt.Sprintf("user%d", u),
					fmt.Sprintf("User%d", u)); err != nil {
					t.Fatalf("seeding %s: %v", day.Format(time.DateOnly), err)
				}
			}
		}
	}
}

func makeDay(t *testing.T, pool *pgxpool.Pool, cfg Config, day time.Time) {
	t.Helper()
	name := "chat_message_" + day.Format("20060102")
	_, err := pool.Exec(context.Background(), fmt.Sprintf(
		`SET LOCAL TimeZone = 'UTC';
		 CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		partition.QuoteQualified(cfg.Schema, name), partition.QuoteQualified(cfg.Schema, "chat_message"),
		day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")))
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func runPass(t *testing.T, cfg Config) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := pass(context.Background(), cfg, &buf)
	return buf.String(), err
}

func counts(t *testing.T, pool *pgxpool.Pool) (live, folded int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT COUNT(*) FROM chat_message),
		       (SELECT COALESCE(SUM(messages), 0) FROM chat_stats)`).Scan(&live, &folded); err != nil {
		t.Fatal(err)
	}
	return live, folded
}

// The property that matters: folding is count-preserving. Lines leave, but the
// number of them a user sent does not change.
func TestFoldPreservesCounts(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 5)
	beforeLive, beforeFolded := counts(t, pool)
	total := beforeLive + beforeFolded

	cfg.Retention = 5 * 24 * time.Hour
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dropped") {
		t.Errorf("nothing was dropped, so the fold was not exercised:\n%s", out)
	}
	if !strings.Contains(out, "counts agree") {
		t.Errorf("the totals did not agree after a clean fold:\n%s", out)
	}

	afterLive, afterFolded := counts(t, pool)
	if afterLive+afterFolded != total {
		t.Errorf("messages went missing across the fold: %d live + %d folded != %d",
			afterLive, afterFolded, total)
	}
	if afterFolded == 0 {
		t.Error("chat_stats is empty — the fold dropped history without recording it")
	}
	if afterLive >= beforeLive {
		t.Errorf("live rows did not shrink: %d -> %d", beforeLive, afterLive)
	}
}

// chat_stats has to carry a name, because users is written only by the economy
// and is empty whenever the economy is off. A folded total nobody can attach a
// person to is not an analytics table.
func TestFoldRecordsNamesAndSpan(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 2)
	cfg.Retention = 5 * 24 * time.Hour
	if out, err := runPass(t, cfg); err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}

	var login, display string
	var messages, chars, first, last int64
	if err := pool.QueryRow(context.Background(), `
		SELECT login, display, messages, chars, first_ts, last_ts
		  FROM chat_stats WHERE user_id = 'u0'`).Scan(
		&login, &display, &messages, &chars, &first, &last); err != nil {
		t.Fatalf("no chat_stats row for u0: %v", err)
	}
	if login != "user0" || display != "User0" {
		t.Errorf("login/display = %q/%q want user0/User0", login, display)
	}
	if messages == 0 || chars == 0 {
		t.Errorf("messages=%d chars=%d want both non-zero", messages, chars)
	}
	// Five days folded, two lines a day.
	if messages != 10 {
		t.Errorf("messages=%d want 10 (5 folded days x 2 lines)", messages)
	}
	if first >= last {
		t.Errorf("first_ts=%d last_ts=%d want a real span across several folded days", first, last)
	}
}

func TestFoldIsIdempotent(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 5)
	cfg.Retention = 5 * 24 * time.Hour

	if out, err := runPass(t, cfg); err != nil {
		t.Fatalf("first pass: %v\n%s", err, out)
	}
	_, foldedAfterFirst := counts(t, pool)

	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("second pass: %v\n%s", err, out)
	}
	if strings.Contains(out, "dropped") {
		t.Errorf("second pass dropped something; there was nothing left to drop:\n%s", out)
	}

	_, foldedAfterSecond := counts(t, pool)
	if foldedAfterSecond != foldedAfterFirst {
		t.Errorf("chat_stats changed on a re-run: %d -> %d — counts were folded twice",
			foldedAfterFirst, foldedAfterSecond)
	}
}

// The crash this has to survive: the fold transaction commits, then the process
// dies before the DROP. The partition is detached, so it is invisible to SELECT
// against chat_message, but it is still in pg_dump — and its counts are already
// in chat_stats. Re-folding it would double every one of them.
func TestFoldSurvivesCrashAfterCommit(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 5)
	cfg.Retention = 5 * 24 * time.Hour
	ctx := context.Background()

	// Fold, but stop short of dropping — exactly the state a crash leaves.
	folded, err := foldExpired(ctx, pool, cfg, time.Now().UTC().Add(-cfg.Retention), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(folded) == 0 {
		t.Fatal("nothing folded, so the crash path was never entered")
	}
	_, statsBefore := counts(t, pool)

	var orphans int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM chat_folded f
		  JOIN pg_class c ON c.relname = f.name
		 WHERE NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans == 0 {
		t.Fatal("no detached-but-undropped partitions, so this is not the crash state")
	}

	// The next pass must clean the orphans up without re-folding them.
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("recovery pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already folded, skipping") && !strings.Contains(out, "dropped") {
		t.Errorf("the recovery pass neither skipped nor dropped:\n%s", out)
	}
	if _, statsAfter := counts(t, pool); statsAfter != statsBefore {
		t.Errorf("chat_stats moved during recovery: %d -> %d — the orphan was folded twice",
			statsBefore, statsAfter)
	}
}

// -dry-run has to be read-only. ADR-0002 records that the ledger's was not, the
// first time, because RegisterParent provisions the premake window on the spot
// rather than merely recording the parent.
func TestDryRunChangesNothing(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 5)
	cfg.Retention = 5 * 24 * time.Hour
	ctx := context.Background()

	var childrenBefore int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_inherits WHERE inhparent = $1::regclass`,
		partition.QuoteQualified(cfg.Schema, "chat_message")).Scan(&childrenBefore)
	liveBefore, foldedBefore := counts(t, pool)

	cfg.DryRun = true
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would fold") {
		t.Errorf("the dry run reported no folds to do:\n%s", out)
	}

	var childrenAfter int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_inherits WHERE inhparent = $1::regclass`,
		partition.QuoteQualified(cfg.Schema, "chat_message")).Scan(&childrenAfter)
	liveAfter, foldedAfter := counts(t, pool)

	if childrenAfter != childrenBefore {
		t.Errorf("partition count changed under -dry-run: %d -> %d", childrenBefore, childrenAfter)
	}
	if liveAfter != liveBefore || foldedAfter != foldedBefore {
		t.Errorf("rows changed under -dry-run: live %d -> %d, folded %d -> %d",
			liveBefore, liveAfter, foldedBefore, foldedAfter)
	}
}

// An erasure request has to reach folded history as well as live rows, or the
// count of what someone said outlives the request to remove it.
func TestPurgeUserRemovesLiveAndFoldedHistory(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 10, 3)
	cfg.Retention = 5 * 24 * time.Hour
	ctx := context.Background()

	if out, err := runPass(t, cfg); err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}

	// u0 must have both halves for the purge to be worth testing.
	var live, folded int64
	pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM chat_message WHERE user_id = 'u0'),
		       (SELECT COALESCE(SUM(messages), 0) FROM chat_stats WHERE user_id = 'u0')`).Scan(&live, &folded)
	if live == 0 || folded == 0 {
		t.Fatalf("u0 has live=%d folded=%d; the fixture needs both", live, folded)
	}

	cfg.PurgeUser = "u0"
	var buf bytes.Buffer
	if err := purgeUser(ctx, cfg, &buf); err != nil {
		t.Fatalf("purge: %v\n%s", err, buf.String())
	}

	pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM chat_message WHERE user_id = 'u0'),
		       (SELECT COALESCE(SUM(messages), 0) FROM chat_stats WHERE user_id = 'u0')`).Scan(&live, &folded)
	if live != 0 || folded != 0 {
		t.Errorf("after purge u0 still has live=%d folded=%d", live, folded)
	}

	// Nobody else is collateral.
	var others int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM chat_message WHERE user_id <> 'u0'`).Scan(&others)
	if others == 0 {
		t.Error("the purge took other users' messages with it")
	}

	// And the next pass notes the discrepancy rather than refusing to run: a
	// purge legitimately moves chat_folded and chat_stats apart, which is why the
	// comparison is a report and not a gate.
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("pass after purge: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOTE: chat_folded recorded") {
		t.Errorf("the pass after a purge did not note the difference:\n%s", out)
	}
}

func TestPurgeDryRunChangesNothing(t *testing.T) {
	cfg, pool := harness(t)
	seedAcrossDays(t, pool, cfg, 3, 2)
	ctx := context.Background()

	liveBefore, _ := counts(t, pool)
	cfg.PurgeUser, cfg.DryRun = "u0", true
	var buf bytes.Buffer
	if err := purgeUser(ctx, cfg, &buf); err != nil {
		t.Fatalf("purge dry run: %v", err)
	}
	if !strings.Contains(buf.String(), "would delete") {
		t.Errorf("dry run said nothing about what it would do:\n%s", buf.String())
	}
	if liveAfter, _ := counts(t, pool); liveAfter != liveBefore {
		t.Errorf("rows changed under -dry-run: %d -> %d", liveBefore, liveAfter)
	}
}

// Rows that land on a day with no child go to chat_message_default, where
// retention can never reach them. -backfill is the only thing that rescues them.
func TestBackfillRescuesTheDefaultPartition(t *testing.T) {
	cfg, pool := harness(t)
	ctx := context.Background()

	// A day far enough back that migration 00006's premake window never covered
	// it, so the row has nowhere to go but the default partition.
	day := time.Now().UTC().AddDate(0, 0, -20).Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (ts, ts_at, room_id, msg_id, user_id, login, display, text)
		VALUES ($1::bigint, to_timestamp($1::bigint), 'room1', 'stranded', 'u0', 'user0', 'User0', 'hi')`,
		partition.QuoteQualified(cfg.Schema, "chat_message")), day.Add(12*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	var stranded int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+
		partition.QuoteQualified(cfg.Schema, "chat_message_default")).Scan(&stranded)
	if stranded != 1 {
		t.Fatalf("default partition holds %d rows, want 1 — the fixture did not strand anything", stranded)
	}

	cfg.Backfill = true
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("backfill pass: %v\n%s", err, out)
	}

	pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+
		partition.QuoteQualified(cfg.Schema, "chat_message_default")).Scan(&stranded)
	if stranded != 0 {
		t.Errorf("%d rows still in chat_message_default after -backfill", stranded)
	}

	// The row moved rather than vanished, and it kept every column.
	var text, login string
	if err := pool.QueryRow(ctx,
		`SELECT text, login FROM chat_message WHERE msg_id = 'stranded'`).Scan(&text, &login); err != nil {
		t.Fatalf("the stranded row did not survive the backfill: %v", err)
	}
	if text != "hi" || login != "user0" {
		t.Errorf("row came back as text=%q login=%q — the backfill dropped columns", text, login)
	}
}
