#!/bin/sh
# Makes a database backup every BACKUP_INTERVAL_HOURS and deletes backups older than
# BACKUP_KEEP_DAYS. Runs inside the "backup" container (see docker-compose.yml).
#
# Backups contain ALL messages and the password hashes: they are as sensitive as the
# database itself. Files are readable by their owner only, and should also be copied
# to a second place (another disk or machine) regularly: see docs/HOSTING.md.
set -eu
umask 077 # new files: owner read/write only

mkdir -p /backups
echo "backup: every ${BACKUP_INTERVAL_HOURS} h, keeping ${BACKUP_KEEP_DAYS} days, into ./backups"

while true; do
  stamp=$(date -u +%Y-%m-%d_%H%M%S)
  tmp="/backups/.vianden-$stamp.dump.partial"
  # --format=custom: compressed, and restorable with pg_restore (see docs/HOSTING.md).
  if pg_dump --format=custom --file="$tmp"; then
    mv "$tmp" "/backups/vianden-$stamp.dump"
    echo "backup: wrote vianden-$stamp.dump ($(du -h "/backups/vianden-$stamp.dump" | cut -f1))"
  else
    rm -f "$tmp"
    echo "backup: FAILED at $stamp" >&2
  fi
  find /backups -name 'vianden-*.dump' -mtime +"$BACKUP_KEEP_DAYS" -delete
  sleep $((BACKUP_INTERVAL_HOURS * 3600))
done
