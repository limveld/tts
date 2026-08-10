#!/usr/bin/env bash
#
# Daily logical backup of the bot's Postgres database.
#
# pg_dump -Fc (custom format: compressed, and pg_restore can pick objects out of
# it) into ~/Library/Application Support/tts/backups, keeping 14 days. This is
# the only piece of the "durability" motivation that phase 1 actually delivers,
# which is why it ships with the backend rather than after it.
#
# A backup that has never been restored is not a backup. To check one:
#
#   createdb tts_restore_check
#   pg_restore -d tts_restore_check ~/Library/Application\ Support/tts/backups/tts-<stamp>.dump
#   store-migrate -from "$TTS_DATABASE_URL" -to postgres:///tts_restore_check -verify-only
#   dropdb tts_restore_check
#
# Env:
#   TTS_DATABASE_URL  the postgres:// DSN to dump (required)
#   TTS_BACKUP_DIR    override the destination directory
#   TTS_BACKUP_DAYS   override the 14-day retention
set -euo pipefail

DSN="${TTS_DATABASE_URL:-}"
BACKUP_DIR="${TTS_BACKUP_DIR:-$HOME/Library/Application Support/tts/backups}"
KEEP_DAYS="${TTS_BACKUP_DAYS:-14}"

if [ -z "$DSN" ]; then
  echo "pg-backup: TTS_DATABASE_URL is not set — nothing to back up" >&2
  exit 1
fi
case "$DSN" in
  postgres://*|postgresql://*) ;;
  *)
    echo "pg-backup: TTS_DATABASE_URL is not a postgres:// DSN ($DSN) — the bot is on SQLite, so there is nothing here to dump" >&2
    exit 1
    ;;
esac

mkdir -p "$BACKUP_DIR"
STAMP="$(date +%Y%m%d-%H%M)"
OUT="$BACKUP_DIR/tts-$STAMP.dump"

# Dump to a .part file and rename on success, so an interrupted run never leaves
# a truncated file that looks like a good backup.
pg_dump -Fc --no-owner --no-acl -f "$OUT.part" "$DSN"
mv "$OUT.part" "$OUT"

# Prune by age. -mtime +N is "older than N+1 days" in find's arithmetic, so use
# N-1 to keep exactly the last N days.
PRUNED=0
if [ "$KEEP_DAYS" -gt 0 ]; then
  while IFS= read -r old; do
    rm -f "$old"
    PRUNED=$((PRUNED + 1))
  done < <(find "$BACKUP_DIR" -maxdepth 1 -name 'tts-*.dump' -type f -mtime "+$((KEEP_DAYS - 1))")
fi

SIZE="$(du -h "$OUT" | cut -f1 | tr -d ' ')"
KEPT="$(find "$BACKUP_DIR" -maxdepth 1 -name 'tts-*.dump' -type f | wc -l | tr -d ' ')"
echo "pg-backup: $(basename "$OUT") ($SIZE) · pruned $PRUNED · $KEPT kept in $BACKUP_DIR"
