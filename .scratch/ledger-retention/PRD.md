# PRD: Ledger retention — a materialized balance and a partitioned ledger

Status: in progress — ADR written, issues 01–08 filed
Type: prd
Created: 2026-08-11

Tracked as [`issues/01`](issues/01-materialized-balance-dual-write.md) … [`issues/08`](issues/08-docs-and-adr-closeout.md).

## Summary

Make `ledger` expirable. Old itemized history leaves the table on a schedule, bounded and
automatic, driven by `github.com/jirevwe/gopartman` over daily range partitions with a 365-day
horizon. Nobody's marks move when it does.

Two structural changes carry the weight: the balance becomes a materialized column on `accounts`
(so it survives the history it was derived from), and redemption-id idempotency moves off `ledger`
into its own non-partitioned table (so it survives too).

## Motivation

`ledger` is the only append-heavy table: 17,315 rows / 1,968 kB today, +~640 rows/day, 98.6% of
them one-minute accruals. That is ~234k rows and ~27 MB per year, forever — nothing has ever
deleted a ledger row.

**The row count does not justify partitioning and won't for years**, and the PRD says so up front
for the same reason the Postgres PRD did. `SUM(delta) WHERE user_id = $1` over a `ledger_user`-
indexed table is sub-millisecond well past 10M rows, which this table reaches around 2069. The
reason to build it: `jirevwe/gopartman` is ours, and this exercises it against a production
workload with real money in it, where a bug gets noticed. See
[ADR-0002](../../docs/adr/0002-ledger-retention-and-partitioning.md).

What the work does buy, independent of partitioning:

- **Retention becomes possible at all.** Today `Balance` is `SUM(ledger)`, so deleting any ledger
  row is a currency change. That is the actual blocker, and it needs fixing under any retention
  design.
- **A latent bug fixed.** `ledger_ref` lives on the ledger, so removing an old ledger row would
  silently re-arm an old channel-point redemption id.
- **The money invariant becomes checked rather than assumed.** `cmd/pg-partition` reconciles
  `accounts.balance` against `ledger_opening + SUM(ledger)` for every user, and refuses to destroy
  history until that comes back clean.

## Decisions

- **Materialize the balance on `accounts`.** Reverses ADR-0001's "no materialized balance". The
  premise (the `SUM` is free) holds against one indexed table, not ~370 daily partitions that
  `Balance`/`Leaderboard` cannot prune — they filter on `user_id` and never on time.
- **Balance goes on `accounts`, not on the ledger row.** A `balance_after` running total read as
  `ORDER BY id DESC LIMIT 1` is *not* O(1) on a partitioned table (MergeAppend across every child),
  and dropping an inactive user's last partition would take their balance with it.
- **Dual-write, then switch.** One migration adds, backfills and maintains the column while reads
  still use `SUM(ledger)`, gated by a conformance test that the two agree. Reads flip in the next
  issue. It is real currency; the column proves itself first.
- **Ref idempotency moves to `ledger_refs`** (non-partitioned). On a partitioned `ledger` the
  unique index would have to include the partition key, degrading the guarantee to once-per-day.
- **Retention is a fold, not a delete.** `DETACH` + fold per-user totals into `ledger_opening` in
  one transaction; `ledger_folded`'s PK makes it idempotent and crash-recoverable. The marker is
  ours, not `partman.partitions.status`.
- **gopartman only provisions.** `HookSkip` plus a non-zero `RetentionPeriod`, so `Sweep` never
  touches the ledger. (Zero is not "off" — it would sweep every past partition.)
- **`ts_at TIMESTAMPTZ NOT NULL`, written by every INSERT.** Generated columns, expression keys and
  `BEFORE INSERT` triggers are each independently illegal here; see the ADR. No `DEFAULT`, so a
  missed insert path fails loudly instead of mis-bucketing.
- **Daily partitions, 365-day horizon, `Premake: 14`.**
- **Maintenance is `cmd/pg-partition` under launchd at 05:15** — after the 05:00 `pg_dump`, so the
  worst-case recovery from a fold bug is a backup taken fifteen minutes prior.
- **SQLite gets the behavioral tables and no retention.** `ledger_folded` is created and never
  written, per the `accounts` precedent in `00002`.

## Architecture

```
                       reads                             writes
  Balance      ──> accounts.balance  (1 row, O(1))    every ledger INSERT is paired with
  Leaderboard  ──> accounts JOIN users               applyDelta() in the same transaction
                   WHERE balance > 0
                                                      Credit(ref) ──> ledger_refs  (idempotency)
  ledger  PARTITION BY RANGE (ts_at)
    ledger_20260814  ledger_20260815  …  ledger_default
         │
         │  cmd/pg-partition, launchd 05:15 (after pg-backup 05:00)
         ├─ gopartman: Migrations → RegisterParent → ImportExisting → Maintain
         │             (provisioning only: HookSkip)
         └─ ours:      foldExpired → reconcile → dropFolded
                          │             │            │
                   DETACH+fold in   balance ==   only if reconcile
                   ONE tx, record   opening +    came back empty
                   ledger_folded    SUM(ledger)
                          ↓
                   ledger_opening   (the audit anchor: what was dropped)
```

## Schema

| Table | Change |
|---|---|
| `accounts` | **`balance BIGINT NOT NULL DEFAULT 0`** + partial index `(balance DESC) WHERE balance > 0` |
| `ledger` | **`ts_at TIMESTAMPTZ NOT NULL`**, PK `(id, ts_at)`, `PARTITION BY RANGE (ts_at)`; loses the `ledger_ref` unique index (PG only for the partitioning; the index drop is both dialects) |
| **`ledger_refs`** | **new**, both dialects — `ref` PK, holds redemption idempotency |
| **`ledger_opening`** | **new**, both dialects — per-user total of history that has been dropped |
| **`ledger_folded`** | **new**, both dialects — which partitions were folded; SQLite never writes it |

Timestamps stay `BIGINT` unix seconds everywhere except `ledger.ts_at`, which exists only because
gopartman cannot partition on an epoch column. `ts_at` is Postgres-only; SQLite's `ledger` never
gets it, and `cmd/store-migrate` treats it as a derived column on the Postgres side following the
existing `game_rounds.state::jsonb` precedent.

## Work breakdown

Eight issues, each independently shippable and leaving `go build/vet/test ./...` green.

| # | Issue | Depends on |
|---|---|---|
| [01](issues/01-materialized-balance-dual-write.md) | `accounts.balance`, dual-write only (migration `00003`) | — |
| [02](issues/02-switch-balance-reads.md) | Switch `Balance`/`balanceTx`/`Leaderboard` to the column | 01 |
| [03](issues/03-ledger-refs-opening-folded.md) | `ledger_refs`/`ledger_opening`/`ledger_folded` (migration `00004`) | 02 |
| [04](issues/04-partition-the-ledger.md) | Convert `ledger` to `PARTITION BY RANGE (ts_at)` (migration `00005`) | 03 |
| [05](issues/05-gopartman-provisioning.md) | Add gopartman; `cmd/pg-partition` provisioning half | 04 |
| [06](issues/06-fold-reconcile-drop.md) | The fold, the reconcile gate, and the drop | 05 |
| [07](issues/07-deploy-launchd-mise.md) | launchd agent at 05:15, `service.sh` target, mise tasks | 06 |
| [08](issues/08-docs-and-adr-closeout.md) | `docs/postgres.md` retention section, ADR Consequences | 07 |

## Tests

- **`MaterializedBalanceMatchesLedger` (issue 01)** — the load-bearing one. A randomized workload of
  `Credit`/`Grant`/`Spend`/`Transfer` (clamped claw-backs, refused overdrafts, repeated refs) over
  ~50 users, then assert for every user that
  `accounts.balance == COALESCE(ledger_opening.delta, 0) + SUM(ledger.delta)`. Both dialects. Issue
  02 rests on it.
- **The existing ref tests pass unmodified** through issues 01–03. `CreditIdempotentRef` and
  `ConcurrentCreditWithSameRefCreditsOnce` moving to a new mechanism without changing is the proof
  the swap is sound.
- **Schema shape (issue 04)** — `relkind='p'`, `partstrat='r'`, the default partition is named
  exactly `ledger_default`, and every row satisfies `ts_at = to_timestamp(ts)`.
- **Fold behavior (issue 06)** — `TestFoldKeepsReconcileClean`, `TestFoldIsIdempotent`,
  `TestFoldSurvivesCrashAfterCommit` (commit the tx, skip the DROP, re-run), and
  `TestDropIsGatedOnReconcile` (corrupt one balance, assert nothing is dropped).
- **Round trip (issues 01, 03)** — `cmd/store-migrate` pg → sqlite → pg with `verify` passing,
  proving balances and opening rows survive a rollback.
- **Manual/live (issue 07)** — `!marks` and `!leaderboard` in chat; a channel-point reward redeemed
  twice credits once.

## Out of scope

- Retention on SQLite. It is the dev/test backend; a money-touching prune path that only runs in
  tests is never really tested.
- Archiving folded partitions into a separate schema. They are dropped; the 05:00 dump is the
  archive.
- Querying folded history. Once a partition is dropped, `ledger_opening` holds a total, not an
  itemization.
- Partitioning anything other than `ledger`. Every other table is an upsert-in-place tally of ≤114
  rows.
- Using gopartman's retention/`Sweep` path, `PartitionData` drain as a routine step (it is a
  recovery tool), tenant partitioning, or its `Manager.Start` ticker.
- Backfilling `balance_after` per ledger row. See the ADR for why it is not the O(1) mechanism.
- PITR/WAL archiving. Still a daily `pg_dump`.

## References

- ADR: [`docs/adr/0002-ledger-retention-and-partitioning.md`](../../docs/adr/0002-ledger-retention-and-partitioning.md)
- The decision this reverses: [`docs/adr/0001-postgres-backend.md`](../../docs/adr/0001-postgres-backend.md) — "No materialized balance"
- Money paths: `store/postgres/points.go`, `store/sqlite/points.go` (`lockAccount`, `balanceTx`,
  `touchAccount`, `Balance`, `Credit`, `Grant`, `Spend`, `Transfer`, `Leaderboard`)
- The contract: `store/storetest/storetest.go`, `store/storetest/points.go`
- Cutover tool that must keep working: `cmd/store-migrate/{main,copy,verify}.go`
- Deploy precedent to copy: `deploy/pg-backup.sh`, `deploy/com.rtukpe.tts-pg-backup.plist.template`,
  the `pgbackup` arm of `deploy/service.sh`
- Operations: `docs/postgres.md`
- Library: `github.com/jirevwe/gopartman` — `ParentConfig`, `Manager.{RegisterParent,ImportExisting,Maintain}`,
  `Migrations()`, `HookSkip`
