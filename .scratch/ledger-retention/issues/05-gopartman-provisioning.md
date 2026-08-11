# Add gopartman; cmd/pg-partition provisioning half

Status: ready-for-agent
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 04 · Unblocks: 06

## Summary

Add the dependency and build the half of `cmd/pg-partition` that creates partitions. No fold, no
drop, nothing destructive — this issue can only ever add tables.

## Prerequisite

**gopartman has no LICENSE file.** That is why pkg.go.dev renders no documentation for it, and
strictly it means no grant of rights to use it. Add one upstream (it is our repo) before taking the
dependency here. One commit, and it un-breaks `go doc`.

## Decisions

- **Pin a tag and re-read `manager.go` at that tag.** The design work read `main`; the only
  published tag is `v0.1.0` and its `CHANGELOG` still says "Unreleased". Verify the API before
  writing against it — `ParentConfig`'s fields, `New`'s required options, `Migrations()`.
- **gopartman provisions and nothing else.** `WithHook(func(…) HookDecision { return HookSkip })`
  so `Sweep` can never touch the ledger, and **a non-zero `RetentionPeriod` anyway**: with zero,
  `ListExpiredPartitions`' `bounds_to <= now` filter catches every past partition and the default
  `HookDrop` deletes them. Zero is not "off", it is "delete everything".
- **`Maintain`, not `Start`.** launchd is the scheduler; the library's internal ticker would be a
  second one. (Convoy's integration makes the same call — its comment reads "Asynq drives Maintain;
  do not also run the library ticker".)
- **Its own `pgxpool`, not the store's `*sql.DB`.** gopartman requires `*pgxpool.Pool`. This is one
  of the four reasons the work lives in `cmd/` rather than in the bot.
- **`ImportExisting`'s `Skipped` slice is logged loudly and is an error in tests.** A skipped
  partition is one that never expires — the silent failure mode this whole design has to avoid.
- **`-backfill` is a recovery tool, not a routine step.** It exists for two situations: the launchd
  agent stopped firing for longer than `Premake` days, or a fresh `store-migrate` cutover dumped a
  year of history into `ledger_default` (migration `00005` derives no children on an empty
  database). It creates the missing back-dated children, then drains the default into them.
- **`-dry-run` prints and changes nothing**, including no `partman` metadata writes.

## Work breakdown

1. `go get github.com/jirevwe/gopartman@<tag>`. Record the real `git diff --stat go.sum` in
   ADR-0002 — the claim there is that `testcontainers` arrives as go.mod-only checksums for ~50
   modules with no Docker code compiled in. Verify rather than repeat it.

2. **`cmd/pg-partition/main.go`** — flags `-db` (default `TTS_DATABASE_URL`), `-retention`,
   `-premake`, `-schema`, `-backfill`, `-dry-run`. `run(ctx, args, out) error` so it is testable.
   Refuse a non-`postgres://` DSN with a clear message rather than failing obscurely — mirror
   `deploy/pg-backup.sh`'s preflight.

3. **`cmd/pg-partition/partition.go`**

   ```go
   type Config struct {
       DSN       string
       Schema    string        // "public"
       Interval  time.Duration // gopartman.PartitionDayInterval
       Premake   int           // 14
       Retention time.Duration // 365 * 24 * time.Hour
       Backfill  bool
       DryRun    bool
   }

   // ensureRegistered applies gopartman's metadata migrations, registers the ledger,
   // and adopts the children migration 00005 created. Idempotent:
   // ErrParentAlreadyExists is the expected outcome from the second run onward.
   func ensureRegistered(ctx context.Context, m *gopartman.Manager, cfg Config) error

   // backfillChildren creates a daily child for every day between the oldest row
   // still in ledger_default and today. gopartman never provisions into the past and
   // PartitionData refuses to drain into a partition that does not exist, so this is
   // the only thing that can rescue stranded rows.
   func backfillChildren(ctx context.Context, pool *pgxpool.Pool, schema string) (created []string, err error)
   ```

   Startup order, all idempotent: `Migrations()` (each `m.SQL` as **one** `Exec` — the files contain
   dollar-quoted plpgsql and must not be split on `;`) → `RegisterParent` (swallow
   `ErrParentAlreadyExists`) → `ImportExisting` (log `Skipped`) → `Maintain`.

4. **`mise.toml`** — `db:partition:build`, `db:partition`, `db:partition:dry`,
   `db:partition:backfill`.

## Tests

`cmd/pg-partition/partition_test.go`, skipping without `TEST_DATABASE_URL` like the rest of the
repo.

- **`TestRegisterIsIdempotentAgainstTheMigration`** — run `ensureRegistered` twice against a schema
  built by `00005`. No duplicate `DEFAULT` partition, no error escaping, and `ImportExisting`'s
  `Skipped` is empty (which is what proves `00005`'s names match gopartman's grammar — the two are
  only coupled by convention, so this test is the coupling).
- **`TestMaintainProvisionsPremake`** — after `Maintain`, children exist for today through
  today+`Premake`.
- **`TestBackfillCreatesBackdatedChildren`** — insert history directly into `ledger_default`, run
  `-backfill`, assert the children exist, are adopted, and the default is empty.
- **`TestDryRunChangesNothing`** — snapshot `pg_class` and `partman.parent_tables` around a
  `-dry-run`; both unchanged.

**Test isolation gotcha.** `partman` is a hardcoded global schema. Per-test temp schemas keep
registrations and advisory locks from colliding, but `DROP SCHEMA … CASCADE` does not remove
`partman.parent_tables` rows — they accumulate in `tts_test` and every later `Maintain` tries to
provision into schemas that no longer exist. Every test here needs:

```go
t.Cleanup(func() {
    pool.Exec(ctx, `DELETE FROM partman.parent_tables WHERE schema_name = $1`, schema)
})
```

Put that helper **here**, not in `store/storetest` — `storetest` is imported by both store packages
and must not grow a gopartman import.

## Acceptance

- `mise run test:all` green; `mise run db:partition:dry` against production prints the registration
  and an empty `Skipped`, and changes nothing.
- `mise run db:partition` twice in a row: the second is a clean no-op.
- `\d+ ledger` shows children out to today+14.
- Nothing in `bot/` or `server/` imports gopartman.
