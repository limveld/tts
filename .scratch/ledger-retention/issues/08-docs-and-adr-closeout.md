# Docs: retention in docs/postgres.md, and closing out ADR-0002

Status: ready-for-agent
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 07 · Unblocks: —

## Summary

Write down where balance lives now, what a fold is, and what to do when something looks wrong. Then
fill in ADR-0002's Consequences with what actually shipped.

## Decisions

- **`docs/postgres.md` gets a Retention section, and its existing ledger prose gets corrected.** The
  file currently describes a schema where balance is `SUM(ledger)`. Leaving that in place is worse
  than never having documented it — grep for `SUM` and for "single source of truth" across
  `docs/` and fix every instance.
- **Document the invariant as the thing to check.** The one-line reconcile query is the most useful
  thing in the section: anyone debugging a marks complaint should run it first.
- **Document the re-cutover wrinkle**, because it is the one non-obvious operational trap: on a
  fresh database `00005` derives no back-dated children (there is no data to derive them from), so
  a full `store-migrate` of a year of history lands entirely in `ledger_default`. The fix is
  `mise run db:partition:backfill`. Someone will hit this during a rollback rehearsal and it will
  look like data loss.
- **ADR Consequences get the real numbers**, including the actual `git diff --stat go.sum` from
  issue 05 rather than the estimate written at design time.

## Work breakdown

1. **`docs/postgres.md` — new "Retention" section:**
   - Where balance lives: `accounts.balance`, written in the same transaction as every ledger row.
     The ledger is history.
   - What runs, when: `cmd/pg-partition` at 05:15 daily, after the 05:00 dump, and why that order.
   - The shape of a pass: provision → fold → reconcile → drop, and that a dirty reconcile stops the
     drop.
   - `psql` recipes:
     - the reconcile query (copy it verbatim from `cmd/pg-partition/verify.go`)
     - `SELECT * FROM ledger_folded ORDER BY through_ts DESC LIMIT 10`
     - listing children and spotting detached orphans
     - `SELECT count(*) FROM ledger_default` — should be 0; non-zero means a missed premake window
   - Recovery: what `-backfill` is for, and the re-cutover wrinkle.
   - What is *not* recoverable: once a partition is dropped, `ledger_opening` holds a total, not an
     itemization. The 05:00 dump is the only itemized archive.

2. **Correct the existing text** in `docs/postgres.md` (and anywhere else in `docs/`) that describes
   balance as `SUM(ledger)` or `accounts` as a pure lock token.

3. **`docs/adr/0002-*.md`** — fill in Consequences with what shipped: the real go.sum stat, whether
   the three `00005` spikes held, any gopartman behavior that differed from the design reading of
   `main`, and the observed timing of a real fold pass.

4. **`.scratch/ledger-retention/PRD.md`** — set the Status line to done with the date, the way the
   postgres-backend PRD does.

5. **Cross-link**: `CONTEXT.md` and `docs/adr/0001` should point at 0002 where they assert the
   no-materialized-balance decision, so a reader of 0001 does not act on a superseded claim.

## Tests

Docs, so the gate is a walkthrough rather than a suite:

- Follow the new section top to bottom against the live database. Every command runs as written.
- The reconcile query pasted from the docs returns the same result as `cmd/pg-partition`'s.
- `grep -rn "SUM(delta)" docs/` turns up nothing that describes it as the balance.

## Acceptance

- A reader who has never seen the epic can answer, from `docs/postgres.md` alone: where is my
  balance, what deleted my old ledger rows, how do I check nothing was lost, and what do I do if it
  was.
- ADR-0002 Consequences contains measured facts, not design-time estimates.
- PRD Status line reflects reality.
