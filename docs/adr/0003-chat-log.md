# ADR-0003: Persisting the chat log, and a second partitioned parent

Date: 2026-08-29
Status: Accepted

## Context

The bot has always seen every line in the channel and kept almost none of them. `IRCClient.serve`
reads each PRIVMSG, `parsePrivmsg` turns it into a fully populated `ChatMessage`, and
`Router.Handle` discards anything that does not start with `!`. Every non-command line the channel
has ever produced was parsed and thrown away.

Four reasons to keep them: an ad-hoc archive, moderation lookups ("what did they say, and was it
deleted"), per-user activity analytics, and substrate for features that cannot be built without
history — a recap, a highlight reel, a `!quote` — none of which can be backfilled later.

**A fifth reason was offered and is withdrawn on the numbers.** The stated motivation included
exercising `gopartman` against more data than the ledger provides. The ledger snapshot behind
ADR-0002 is 16,864 rows over 26 days — 647/day, of which 16,643 are one-minute `accrual` rows. The
human-driven rows (`tts`, `sfx`, `gamble_bet`, `wordle_win`, …) total **221 over 26 days, about
8.5/day**. At a typical command-to-chat ratio that puts this channel somewhere near 60–250 chat
messages a day. **Chat is a lower-rate table than the ledger.** It will not stress-test anything by
volume and this ADR does not pretend otherwise, the way ADR-0002 did not pretend the ledger's row
count justified partitioning it.

What chat does give gopartman is a **second independently-registered parent**: a different fold
shape, a different retention horizon, a different gate, per-parent premake, and two maintenance jobs
whose advisory locks must not collide. None of that was exercised by one table. That is a real
reason and it is a smaller one than "more data", which is why it is written down as the smaller one.

## Decision

**Every PRIVMSG is persisted, and CLEARMSG/CLEARCHAT become tombstones.** The
`twitch.tv/commands` capability was already being requested, so both tombstone commands were
already arriving and being dropped on the floor. USERNOTICE (subs, resubs, raids) is **not**
captured — a deliberate and losing trade, since it is exactly the kind of thing that cannot be
backfilled, made because the alternative was a polymorphic event table with most columns null for
most rows.

A CLEARCHAT with no `target-user-id` is a whole-channel `/clear` and is **ignored**. It tidies the
display and says nothing about the messages; tombstoning a channel's entire history on a routine
mod action would erase the distinction the log exists to record.

**Logging must never stall the IRC read loop.** That loop is single-threaded and is also what
answers `!tts`, so a synchronous INSERT would mean a wedged database makes the bot go deaf. The read
loop does a non-blocking send into a 2048-deep channel; one writer goroutine owns every database
call and batches at 256 rows or 200ms, whichever comes first.

The honest cost: a full buffer **drops** rather than waits. "All chat messages" means "all chat
messages unless the database is unavailable long enough to fill the buffer", which is why the drop
count is logged rather than swallowed. At this channel's rate the buffer is never more than a few
deep; it is sized for a raid burst and for an outage.

Tombstones travel the same channel as the messages, so the writer applies them in the order the read
loop saw them, and it flushes pending rows before applying one. A CLEARMSG that overtook the message
it targets would match no row and the deletion would be lost.

**A tombstone keeps the text.** `deleted_at` and `deleted_by` are set; `text` is untouched. The
moderation question is *what did they say that got them banned*, and redacting answers the one case
the log exists for with a blank. Erasure requests are served by `chat-partition -purge-user`, which
is a real delete of both the live rows and the folded counts — so "delete my data" has a mechanism
behind it rather than hand-written SQL under time pressure.

A CLEARCHAT tombstones only the last 24 hours of that user's lines. Twitch clears the visible
buffer, not the channel's history; tombstoning everything a person ever said would claim more than
actually happened.

**Names are denormalized onto the row.** `users` exists but is written only by the economy
(`bot/economy.go`) and is empty whenever the economy is off, so a join would produce anonymous rows
on a perfectly ordinary configuration. `wordle_wins` and `connections_wins` set this precedent. It
also makes the row carry the name as it was at the time, which is the right answer for an archive.

**Text is stored raw.** `removeEmotes` can always recompute the stripped form from `text` plus
`emotes`; nothing recovers the original from the stripped one.

**Counters are fold-only — the `ledger_opening` pattern, not the `accounts.balance` one.**
`chat_stats` is written exclusively by the maintenance job, never on the write path. The consequence
is a seam: a user's true total is `chat_stats.messages` plus a live `COUNT(*)`, and either half
alone is an undercount.

ADR-0002 paid for a materialized balance because `Balance` and `Leaderboard` are runtime reads that
filter on `user_id` and can never prune partitions. Nothing reads chat totals at runtime — there is
no `!chatstats` and this ADR does not add one — so the same argument does not transfer. Buying an
O(1) read for a query nobody issues would cost a second write per batch, a lock-contention point
under raid load, and the three defending mechanisms a derived value needs. The seam is cheaper than
all of that, and it is documented rather than hidden.

**Retention is 90 days of itemized chat, against the ledger's 365.** A year of ledger rows is what
makes "why do I have this many marks" answerable; no moderation question reaches back past a
quarter. Stated plainly: **`pg-backup.sh` keeps fourteen days of dumps, so from day 91 a line's text
is gone for good.** ADR-0002's "the partitions are dropped and the 05:00 dump is the archive" is
true for the ledger only in the sense that a fold bug has a two-week recovery window. For chat the
dump is that recovery window and nothing more. `chat_stats` keeps the counts; nothing keeps the
words.

**Nothing gates the drop.** `cmd/pg-partition` refuses to destroy ledger history until every balance
has been proved against it, because the ledger is money and a wrong number there is somebody's
marks. The chat pass compares `chat_folded`'s row counts against `chat_stats`' totals and **reports**
the difference. Halting retention over a message count nothing reads would stop the log expiring for
no one's benefit — and the equality is not one the tooling could insist on anyway, because
`-purge-user` legitimately moves the two totals apart. A gate would turn every erasure request into a
stuck nightly job.

**The shared machinery moved to `internal/partition`; the two folds stayed put.** Provisioning,
gopartman's `HookSkip` configuration, bounds parsing, the UTC handling, identifier quoting, the
advisory lock, the claim→DETACH→aggregate→commit skeleton, the orphan sweep and the default-partition
rescue are one implementation used twice. The arithmetic and the gate are not shared, because they
are the parts that differ.

Duplication was the alternative and was rejected on this repo's own record: ADR-0002 lists the
UTC-versus-session-TimeZone bug, the `-dry-run` that was not read-only, the advisory lock that has to
be gopartman's own, and the backfill ordering Postgres forces as four things the work found that the
design missed. A second hand-written copy re-opens every one. The safety argument for touching a
money tool is that `cmd/pg-partition`'s seven tests pass unchanged after the extraction.

**`chat-partition` runs at 05:30**, after the 05:00 dump and the ledger's 05:15 pass. gopartman's
advisory lock is per-parent so the two cannot deadlock, but they both run partman's migrations and
call `Maintain`, and keeping them apart means a failure has one obvious owner.

**SQLite gets all three tables and no partitioning**, exactly as ADR-0002 left the ledger.
`chat_folded` is created and never written. `chat_stats` stays empty, and that is correct rather
than broken: nothing is ever dropped there, so the live `COUNT` is the whole total.

**No new dependencies.** The count stays at seven.

## Consequences

**Good.** The channel's history exists. Moderation can answer "what did they say and was it deleted"
without the text having been redacted by the deletion it is asking about. The write path is
unchanged in cost — no second write, no new lock — and the read loop provably cannot be stalled by
the database, which is asserted directly by a test that runs with no writer at all. Erasure has a
mechanism. And gopartman now has a second parent with a different fold, a different horizon and a
different gate, which is the only claim about partitioning this work actually supports.

**Costs.** "All chat messages" is now a claim with a caveat attached, visible only in a log line
counting drops. A user's message total requires two queries and the seam is easy to get wrong from
`psql` — there is deliberately no view hiding it, so the first person to forget the live half will
undercount. Ninety-day-old text is unrecoverable, with fourteen days of dumps behind it. A
money-critical tool was refactored for a non-money feature's benefit. And `ts_at`'s relationship to
`ts` is now enforced by convention and a test on a second table, because Postgres still permits no
other way.

**What the work found that the design missed.** Two things, each of which would have been a bug:

- `cmd/store-migrate` copies rows as `[]any`, and SQLite returns `INTEGER 0/1` for the four boolean
  role columns while Postgres declares them `BOOLEAN`. pgx refuses `int64` for a bool parameter, and
  refuses at *encode* time — before any cast in the statement could help. The fix is
  `::int::boolean`, which changes what pgx is asked for rather than what it is given. Caught by
  extending `seedSource`, not by review.
- partman's schema is a hardcoded global shared by every parent, so a second tool that applies its
  migrations makes them concurrent. Two processes doing it at once fail one with `tuple concurrently
  updated` (XX000). The scheduled agents are fifteen minutes apart and cannot collide; what found it
  was `go test ./...` running the two commands' suites in parallel. The migration step now takes a
  global advisory lock of our own — gopartman's is per-parent, and what needs protecting is the
  schema the parents share.
- `Maintain` maintains **every parent in partman's registry**, not the one it was asked about — and
  that registry is global, so each tool's tick takes the per-parent advisory lock on the *other*
  tool's table for a moment. `Lock` used `pg_try_advisory_lock` once and treated a refusal as
  "another pass is running", which conflated a sibling mid-tick with a genuine overlap and would
  silently skip a night's retention. It now retries for fifteen seconds: long enough to outlast a
  tick, far too short to outlast a fold, which is exactly the distinction that needed drawing.

Both have the same shape, and it is the one thing worth carrying forward from this work: **adding a
second parent turned single-process assumptions into races, and nothing about "register another
table" said it would.**

**Not delivered.** USERNOTICE capture — sub, resub, gift and raid history is still being dropped on
the floor every day this stays unbuilt, and unlike everything else here it cannot be recovered
later. Any chat-facing read: no `!chatstats`, no participation leaderboard. Archiving folded
partitions anywhere. Retention on SQLite. A view over the `chat_stats`/live seam.

**Honored.** ADR-0001's "the bot is sole owner of the database" — `cmd/chat-partition` is a
maintenance job in `cmd/`, alongside `store-migrate` and `pg-partition`, not a second writer inside
`bot/`. ADR-0001's `ts BIGINT` convention, with `ts_at` added beside it exactly as 00005 did. And the
`store/storetest` contract: every behavioral table exists on both backends, and the four chat methods
are conformance-tested against both.

## References

- ADR-0002: [ledger retention and partitioning](0002-ledger-retention-and-partitioning.md) — the
  pattern this follows, and the four traps it records
- Operations: `docs/postgres.md`
- The shared machinery: `internal/partition`
- The contract: `store/storetest`
