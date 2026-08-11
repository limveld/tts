# ADR-0002: A materialized balance, and a partitioned ledger with balance-preserving retention

Date: 2026-08-11
Status: Accepted
Supersedes: the "No materialized balance" decision in [ADR-0001](0001-postgres-backend.md)

## Context

`ledger` is the only append-heavy table in the schema: 17,315 rows / 1,968 kB, growing ~640
rows/day — 98.6% of them one-minute `accrual` rows, one per present viewer while the stream is
live. That is ~234k rows and ~27 MB per year, forever, because nothing in the repo has ever
deleted a ledger row. The only retention that exists is `find -mtime` over `pg-backup.sh`'s dumps.

We want the ledger to be expirable: old itemized history leaves the table on a schedule, bounded
and automatic. `github.com/jirevwe/gopartman` provisions and manages the partitions.

**The row count does not justify this and won't for years.** `SUM(delta) WHERE user_id = $1` over
a `ledger_user`-indexed table stays sub-millisecond well past 10M rows, which this table reaches
around 2069. ADR-0001 said so and was right. The honest reason to do it anyway: `jirevwe/gopartman`
is ours, and this exercises it against a production workload with real money in it, where a bug
gets noticed. That is a legitimate reason to spend a dependency — the maintenance burden was
already ours — but it is not a performance argument, and this ADR does not pretend otherwise.

Two properties of the current schema make retention non-trivial, and they drive everything below.

**Balance is derived.** `Balance` is `SELECT COALESCE(SUM(delta),0) FROM ledger WHERE user_id = $1`.
So dropping an old partition silently changes people's marks — including escrowed `!g` buy-ins,
which are real liabilities. Retention cannot be a `DROP` while that holds.

**`ledger.ts` is `BIGINT` unix seconds**, deliberately (ADR-0001: one code path across both
backends, a straight value copy at cutover). gopartman partitions on a `date`/`timestamptz` column
and lists *"epoch (integer-time) partition columns"* as an explicit non-goal.

## Decision

**Materialize the balance on `accounts`.** This reverses ADR-0001's "No materialized balance… the
`SUM` is free. Revisit only on profiling." The premise was true and stops being true here: the
`SUM` is free against one indexed table, not against ~370 daily partitions that `Balance` and
`Leaderboard` cannot prune, because they filter on `user_id` and never on time. Profiling didn't
change the answer; partitioning changed the question.

It buys more than the read cost. A materialized balance decouples *what someone has* from *which
history rows still exist*, which is the entire point of retention. Once balance is a column,
dropping a partition cannot move anyone's marks, and the fold machinery below leaves the
money-correctness path entirely.

`accounts` was introduced by ADR-0001 as a pure lock token specifically so that nothing derived
could drift. That concern is real and is answered by construction rather than by abstinence:
`accounts.balance` is written in the same transaction as the ledger row it corresponds to, and
`cmd/pg-partition` refuses to destroy history until it has proved
`accounts.balance = ledger_opening + SUM(ledger)` for every user. The invariant is checked, not
assumed.

**The balance lives off the ledger, not on it.** A `balance_after` running total per ledger row —
read as `… WHERE user_id = $1 ORDER BY id DESC LIMIT 1` — is *not* O(1) once partitioned: with no
time predicate to prune on, Postgres MergeAppends across every child's index to find the global
newest row. It also breaks under retention, since dropping the partition holding an inactive
user's last row takes their balance with it. `accounts` is unpartitioned and is already the row
every write path locks.

**Introduced by dual-write, then switch.** The column is added, backfilled and maintained on every
write for one migration while reads still use `SUM(ledger)`, gated by a conformance test asserting
the two agree after a randomized workload. Only then do the reads flip. This is real currency; the
column proves itself before anything trusts it.

**Ref idempotency moves to its own table.** `ledger_ref` is `UNIQUE (ref) WHERE ref IS NOT NULL`
and makes a channel-point redemption credit at most once. A partitioned table's unique index must
include the partition key, so on `ledger` it would degrade to "at most once *per day*" — and the
failure mode is a duplicate credit at a day boundary, exactly where Twitch's re-delivery window
lands. `ledger_refs` is non-partitioned and holds the guarantee. It is also a latent-bug fix:
idempotency is a property of the redemption id, not of the ledger row it produced, so it has to
outlive that row.

**Retention is a fold, not a delete.** Before a partition leaves, its per-user `SUM(delta)` folds
into `ledger_opening` in the *same transaction* as the `DETACH` — Postgres has transactional DDL,
so it costs nothing and keeps `accounts.balance = opening + SUM(ledger)` true at every instant.
`ledger_folded`'s primary key makes the fold idempotent and crash-recoverable; a fold that died
after `COMMIT` is indistinguishable from one that finished. The marker is deliberately *ours* and
not `partman.partitions.status`: coupling a money invariant to another library's bookkeeping means
a library upgrade can change what the money means.

Note what materializing bought: no user-visible balance depends on that transaction at all. A fold
bug is a reconciliation alert, not someone's marks changing.

**gopartman does one job: provisioning.** It is configured with `HookSkip` so `Sweep` can never
touch the ledger, plus a non-zero `RetentionPeriod` — with zero, `ListExpiredPartitions`' `bounds_to
<= now` filter catches *every* past partition and the default `HookDrop` deletes them. So the
library creates tomorrow's child table and nothing else. Stating that plainly is the cost/benefit
argument in one sentence.

**A `ts_at TIMESTAMPTZ NOT NULL` column, written explicitly by every INSERT.** `ts` stays as it is.
The three tidier alternatives are all illegal, verified against the PG 18.4 binary we run:

- a stored generated column — `cannot use generated column in partition key`;
- an expression key `PARTITION BY RANGE (to_timestamp(ts))` — PostgreSQL then refuses *any* unique
  constraint including the primary key, and gopartman's `PartitionBy` is a column name anyway;
- a `BEFORE INSERT` trigger to fill it — tuple routing happens before row triggers, so it fails
  with `moving row to another partition during a BEFORE FOR EACH ROW trigger is not supported`, and
  only once a matching bounded child exists. That one would have passed in tests and failed in
  production.

No `DEFAULT` on the column, so a forgotten insert path fails loudly on its first row rather than
silently mis-bucketing history. The primary key becomes `(id, ts_at)`, since a PK must include all
partitioning columns.

**Daily partitions, 365-day horizon, `Premake: 14`.** Daily is a deliberate choice against the
~640-rows-per-child argument; the read cost it would have imposed is what the materialized balance
removes. `Premake: 14` over the default 4 buys two weeks of grace if the launchd agent stops
firing — a missing future partition is not data loss (rows land in `ledger_default` and
`-backfill` recovers them), but the recovery is manual, so make it rare.

**Maintenance is `cmd/pg-partition` under launchd at 05:15**, fifteen minutes after
`pg-backup.sh`. That ordering is a safety property, not a scheduling detail: the 05:00 `pg_dump`
contains every row the 05:15 pass is about to remove. A separate binary rather than a goroutine in
the bot, because `Store.DB()` is documented as `cmd/`-only, the bot may be running on SQLite where
none of this means anything, and gopartman needs a `pgxpool` while the store is `database/sql`.

**SQLite gets the behavioral tables and no retention.** `accounts.balance`, `ledger_refs` and
`ledger_opening` are real on both sides because six conformance-tested methods touch them.
`ledger_folded` is created and never written, following the precedent `accounts` set in
`00002`. SQLite retention does nothing on purpose: it is the dev/test backend, and a money-touching
prune path that only ever runs in tests is a path that is never really tested.

**Dependencies go from 6 to 7**: `github.com/jirevwe/gopartman`. Recorded explicitly, per ADR-0001's
convention, so the near-stdlib budget is consciously spent. gopartman declares
`testcontainers-go/modules/postgres` as a *direct* require even though it is test-only, so `go.sum`
gains go.mod-only checksums for ~50 modules (moby, OTel, logrus). No Docker code is compiled into
the bot.

## Consequences

*(Filled in as the epic lands — see `.scratch/ledger-retention/PRD.md` for current state.)*

**Good.** Balance and leaderboard reads become O(1) and partition-count-independent. The ledger
stops growing without bound, and stops doing so without moving anyone's marks. `ledger_refs` fixes
a latent bug where deleting an old ledger row would silently re-arm an old redemption id. The
`accounts.balance = opening + SUM(ledger)` check makes the money invariant something the tooling
proves on every run rather than something the schema hopes for.

**Costs.** A derived value now exists and can drift — exactly what ADR-0001 avoided. It is
defended by a same-transaction write, a conformance test, and a reconcile gate, which is three
mechanisms where "no derived value" was zero. Every ledger INSERT site now writes three things
(the row, `ts_at`, the balance) instead of one. A seventh dependency, at `v0.1.0`, whose retention
path we deliberately do not use.

**Not delivered.** Retention on SQLite. Archiving to a separate schema (partitions are dropped;
the 05:00 dump is the archive). Any query that reads folded history — once a partition is dropped,
`ledger_opening` holds a total and not an itemization.

**Honored.** ADR-0001's "the bot is sole owner of the database" — `cmd/pg-partition` is a
maintenance job in `cmd/`, alongside `store-migrate`, not a second writer inside `bot/`. And the
`store/storetest` contract: every behavioral table exists on both backends or the suite is testing
two different programs.

## References

- PRD and issues: `.scratch/ledger-retention/`
- Operations: `docs/postgres.md`
- The contract: `store/storetest`
- The library: `github.com/jirevwe/gopartman`
