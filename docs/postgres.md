# Postgres

The bot runs on either of two storage backends, chosen by the `-db` flag (or
`TTS_DATABASE_URL`):

| DSN | Backend |
|---|---|
| `bot.db`, or any bare path | SQLite — the default, no daemon |
| `sqlite:///abs/path.db` | SQLite, spelled as a URL |
| `postgres://…` / `postgresql://…` | Postgres |

Both are permanently supported. SQLite stays the fast, hermetic default for
`go test ./...` and for running the bot without a daemon; Postgres is what the
live channel runs on. One shared conformance suite (`store/storetest`) proves the
two behave identically, and it runs against both in the same `go test ./...`.

Phase 1 is a **local** Postgres on this Mac. That is deliberately not a
deployment win — a local daemon is the same failure domain as a local file, plus
a socket. What it buys is that the hard parts (SQL portability, money safety
under real concurrency, versioned migrations, a verified data cutover) are done,
so moving the instance off-box later is a DSN change rather than a project.

## Install

```sh
mise run db:install    # brew install postgresql@18 && brew services start postgresql@18
mise run db:create     # createdb tts
mise run db:status     # service state + pg_isready
```

Pinned to **`postgresql@18`**. A major-version upgrade (`@18` → `@19`) needs
`pg_upgrade` and takes the bot down for the duration — it is a scheduled task,
never a `brew upgrade` side effect. Check `brew services list` before upgrading
anything Postgres-shaped.

## Dev and test

The Postgres half of the conformance suite is skipped unless a test DSN is set:

```sh
mise run db:test:setup    # createdb tts_test, prints the line to add
```

Then put it in `mise.local.toml` (gitignored):

```toml
[env]
TEST_DATABASE_URL = "postgres:///tts_test"
```

| Command | Behavior |
|---|---|
| `mise run test` | whole suite; prints a banner saying whether Postgres cases will run |
| `mise run test:all` | whole suite with `TTS_REQUIRE_POSTGRES=1`; **fails** if Postgres is down |
| `mise run store:test` | just the store suites and the migrate tool |

The banner matters: `t.Skip` messages are invisible without `-v` and a passing
package's output is swallowed, so without it a green `go test ./...` looks
identical whether or not half the backend was exercised.

Isolation is one temp schema per test, created and dropped around each store. It
is not a temp database (`CREATE DATABASE` serializes on the template and costs
100ms+ each) and not transaction rollback (the implementations open their own
transactions, and rolling back around them would hide the `FOR UPDATE` behavior
that is the single most important thing being tested).

## Migrations

Versioned SQL under `store/{sqlite,postgres}/migrations/`, run by
`pressly/goose/v3` as a library. **They run inside `Open`**, so a fresh database
comes up ready and an existing one is carried forward; there is no separate
migrate step to forget.

Migration `00001` is frozen. It is the pre-migration schema statement for
statement, and because every statement is `IF NOT EXISTS`, applying it to the
live `bot.db` is a genuine no-op that writes only the version row. Tidying it
would break that.

To create or upgrade a schema without copying data:

```sh
TTS_DATABASE_URL='postgres:///tts' mise run db:migrate
```

## Cutover

Run off-stream, with no `!g`, `!wordle` or `!connections` round open. Escrowed
`!g` buy-ins are the one thing in the database a botched cutover could turn into
visible unfairness.

```sh
mise run bot:service:stop

# ⚠️  Archive with VACUUM INTO, never cp — see the WAL trap below.
sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"
sqlite3 bot.db.pre-postgres "SELECT COUNT(*) FROM ledger"   # write this number down

export TTS_DATABASE_URL='postgres:///tts'
mise run db:create
mise run db:migrate
mise run db:cutover     # copies, then verifies; exits non-zero on any mismatch
mise run db:verify      # standalone re-check, for the record

mise run bot:service:install    # bakes TTS_DATABASE_URL into the plist
mise run bot:service:restart
```

`db:cutover` prints per-table counts and then a verification block. **Any
mismatch stops the cutover** — the tool exits non-zero and nothing has changed
yet, so the bot restarts on SQLite.

### ⚠️ The WAL trap

`bot.db` runs in WAL mode, and recent writes live in `bot.db-wal` until a
checkpoint. **`cp bot.db backup.db` copies the main file only and silently
discards every uncheckpointed row** — the archive looks fine and is missing the
most recent marks, gambles and wins. With the bot stopped, either:

```sh
sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"     # preferred
```

or copy `bot.db`, `bot.db-wal` and `bot.db-shm` **together**. `VACUUM INTO` is
preferred because it produces one self-contained, already-checkpointed file.

## Rollback

| When | How |
|---|---|
| Before the restart | Nothing changed. `mise run bot:service:start` — still SQLite. |
| After the restart, before any writes | `unset TTS_DATABASE_URL`, `mise run bot:service:install`, restart. `bot.db` is untouched. |
| After live writes | `mise run db:rollback` (copies Postgres → `bot.db`, verifying), then unset and restart. **Nothing written to Postgres is lost.** |

Worst case, `bot.db.pre-postgres` is a known-good snapshot of the moment before
the cutover.

## Backups

A daily `pg_dump -Fc` LaunchAgent at 05:00, keeping 14 days in
`~/Library/Application Support/tts/backups`:

```sh
mise run db:backup:install    # install the agent
mise run db:backup            # take one now
```

**A backup that has never been restored is not a backup.** To check one:

```sh
createdb tts_restore_check
pg_restore --dbname=tts_restore_check ~/Library/Application\ Support/tts/backups/tts-<stamp>.dump
./bin/store-migrate -from "$TTS_DATABASE_URL" -to postgres:///tts_restore_check -verify-only
dropdb tts_restore_check
```

Phase 1 is daily logical dumps only — no PITR, no WAL archiving. The window of
loss is up to 24 hours.

## What the crash loop at login means

`brew services start postgresql@18` installs its own login agent, and the bot's
agent may well win the race at boot. **There is deliberately no launchd ordering
between them.**

The bot fails fast on a store error (`logger.Fatalf`), launchd's `KeepAlive`
throttles the respawn to roughly 10 seconds, and the bot comes up as soon as
Postgres is listening. So a handful of lines like this in
`~/Library/Logs/tts-bot.err.log` right after login are **normal**:

```
store postgres:///tts: connecting: failed to connect …: connection refused
```

They should stop within a minute. If they don't, Postgres isn't starting —
`mise run db:status`.

This is worth understanding rather than "fixing". A retry loop inside the bot
would mean `openStore` returns a store that isn't connected yet, which turns
every `if r.store == nil { return }` guard in `bot/` into a live code path
serving half-broken behavior to chat. Failing fast and letting launchd retry
keeps "the store works" a precondition instead of a maybe.

## Operating

```sh
mise run db:psql       # psql against $TTS_DATABASE_URL
mise run db:status     # is it up?
mise run db:verify     # compare bot.db against Postgres (during a soak)
```

The `server` process has no database access and gains none — the bot is the sole
owner, and the overlay is fed by pushes from the bot rather than by queries.

Useful now that rounds are real rows rather than a JSON blob in `settings`:

```sql
SELECT game, room_id, to_timestamp(ends_at/1000) AS ends FROM game_rounds;
SELECT reason, COUNT(*), SUM(delta) FROM ledger GROUP BY reason ORDER BY 2 DESC;
```

## Retention

### Where a balance lives

**`accounts.balance` is what someone has. The ledger is how they got there.**

That split is the whole of this section. It used to be that a balance *was*
`SUM(ledger)`; it is now a column, written in the same transaction as every
ledger row by `applyDelta`. The reason is that a balance derived from history
cannot outlive that history, and the ledger is now expirable — old partitions get
folded away and dropped, which would silently change a `SUM(ledger)` balance.
See [ADR-0002](adr/0002-ledger-retention-and-partitioning.md), which reverses
ADR-0001 on this point.

So the invariant to know, and the first thing to run when someone says their
marks look wrong:

```sql
-- Should return no rows, always, on any database at any time.
SELECT a.user_id, a.balance,
       COALESCE(o.delta, 0) + COALESCE(l.total, 0) AS derived
  FROM accounts a
  LEFT JOIN ledger_opening o ON o.user_id = a.user_id
  LEFT JOIN (SELECT user_id, SUM(delta) AS total FROM ledger GROUP BY user_id) l
         ON l.user_id = a.user_id
 WHERE a.balance <> COALESCE(o.delta, 0) + COALESCE(l.total, 0);
```

`ledger_opening` is the per-user total of history that has been *dropped*. It is
not a balance and nothing reads it for money — it exists so the sum above stays
provable after retention has run.

### What runs, and when

`cmd/pg-partition`, daily at 05:15 under launchd, fifteen minutes after the 05:00
`pg_dump`. That ordering is a safety property: this job drops ledger partitions,
and that dump is the only itemized copy of the rows it removes.

```sh
mise run db:partition:dry       # what a pass would do, changing nothing
mise run db:partition           # run one now
mise run db:partition:install   # install the daily agent
mise run db:partition:status
mise run db:partition:logs
```

One pass, in order:

1. **provision** — gopartman creates tomorrow's child table (and 14 days ahead)
2. **fold** — each partition older than the horizon is `DETACH`ed and its
   per-user totals added to `ledger_opening`, *in one transaction*
3. **reconcile** — the query above, for every user
4. **drop** — the folded partitions, **only if reconcile came back clean**

A dirty reconcile stops the drop and exits non-zero. That is the design: a
disagreement means either the balance or the history is wrong, and dropping the
history destroys the evidence of which.

The default horizon is 365 days, the interval is daily. gopartman's own retention
is disabled (`HookSkip`) — it only ever creates partitions, never removes them,
because removal has to fold in the same transaction as the detach and a pre-drop
hook cannot do that.

### Reading the state

```sql
-- What has been folded away, most recent first.
SELECT name, to_timestamp(from_ts) AS from, rows, delta,
       to_timestamp(folded_at) AS folded
  FROM ledger_folded ORDER BY through_ts DESC LIMIT 10;

-- The partitions that exist.
SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
  FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
 WHERE i.inhparent = 'ledger'::regclass ORDER BY c.relname;

-- Should be 0. Anything here arrived on a day with no partition, and retention
-- cannot see it: only registered bounded partitions are expiry candidates.
SELECT COUNT(*) FROM ledger_default;

-- Detached orphans: folded but not dropped, because a run died in between.
-- Invisible to SELECT ... FROM ledger, but still in every pg_dump. The next
-- pass cleans them up.
SELECT c.relname FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE c.relname LIKE 'ledger_2%'
   AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid);
```

### When rows are stranded in `ledger_default`

Two ways it happens: the agent stopped firing for more than 14 days, or a fresh
`store-migrate` cutover copied history into a database whose `00005` had no data
to derive children from. **The second one looks exactly like data loss and is
not** — the rows are all there, in the default partition, they just cannot expire.

```sh
mise run db:partition:backfill
```

That creates the missing children and moves the rows into them. It has to detach
the default partition to do it, because Postgres refuses to create a bounded
partition while the default holds a row that would belong in it — all in one
transaction, so a failure leaves the table as it was.

### What is not recoverable

Once a partition is dropped, `ledger_opening` holds a **total**, not an
itemization. "Why do I have this many marks" is answerable for 365 days and no
further. The 05:00 dump is the only itemized archive beyond that, and it is
pruned at 14 days — so in practice, history older than a year is gone. That is
the trade; nobody's balance changes, but the story behind it stops being
available.

## The chat log

Every line the bot sees is persisted to `chat_message`, along with tombstones for
Twitch's `CLEARMSG` (one message deleted) and `CLEARCHAT` (a ban or timeout).
The bot writes it asynchronously and can never be stalled by the database doing
so — see `bot/chatlog.go` and [ADR-0003](adr/0003-chat-log.md).

Turn it off with `-chat-log=false`. It is on by default because every line it is
off for is a line that cannot be recovered later.

### Retention: 90 days, and what survives

`chat_message` is partitioned by day exactly like `ledger`, maintained by
`cmd/chat-partition` at **05:30** — after the 05:00 dump and the ledger's 05:15
pass.

```sh
mise run chat:partition:dry       # what a pass would do, changing nothing
mise run chat:partition           # run one now
mise run chat:partition:install   # install the daily agent
mise run chat:partition:status
mise run chat:partition:logs
```

One pass, in order:

1. **provision** — gopartman creates tomorrow's child (and 14 days ahead)
2. **fold** — each partition past the horizon is `DETACH`ed and its per-user
   counts added to `chat_stats`, *in one transaction*
3. **report** — `chat_folded`'s row counts are compared against `chat_stats`
4. **drop** — the folded partitions

Note step 3 **reports and does not gate**, which is the one real difference from
the ledger's pass. The ledger refuses to drop anything until every balance is
proved, because it is money. A chat count is not, and `-purge-user` moves the two
totals apart legitimately — so a gate would turn every erasure request into a
stuck nightly job.

> **The dump is not an archive here.** `pg-backup.sh` keeps 14 days; chat
> retention is 90. The dump is the recovery window for a bug in *this job*, not
> a copy of the history it expires. **From day 91 the text is gone for good.**
> `chat_stats` keeps the counts; nothing keeps the words.

### Counting someone's messages

`chat_stats` is written **only** by the fold, so it counts only what has already
been dropped. The live rows are the other half:

```sql
SELECT COALESCE((SELECT messages FROM chat_stats WHERE user_id = :id), 0)
     + (SELECT COUNT(*) FROM chat_message WHERE user_id = :id) AS total;
```

Either half alone is an undercount. There is deliberately no view hiding the
seam — see ADR-0003 for why the counter is not maintained on the write path.

### Moderation lookups

```sql
SELECT to_timestamp(ts), display, text, deleted_by
  FROM chat_message
 WHERE user_id = :id
 ORDER BY ts_at DESC
 LIMIT 50;
```

A tombstoned row keeps its text on purpose: the question is what somebody said
before they were removed, and a redacted row answers it with a blank.
`deleted_by` is `clearmsg` for a single deletion and `clearchat` for a ban or
timeout; a `CLEARCHAT` marks the previous 24 hours of that user's lines, matching
what Twitch actually clears.

### Erasure requests

```sh
mise run chat:purge -- <user-id>        # add -dry-run to see the counts first
```

This is a real delete of both the live rows and the user's `chat_stats` row — a
tombstone is not an erasure, since it deliberately keeps the text. Afterwards the
next nightly pass prints a `NOTE:` about `chat_folded` and `chat_stats`
disagreeing; that is expected and is why the comparison is a report.

### When rows are stranded in `chat_message_default`

Same cause and same fix as the ledger's, one table over:

```sh
mise run chat:partition:backfill
```
