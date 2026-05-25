#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

BACKUP_DIR="${BACKUP_DIR:-./backups}"
mkdir -p "$BACKUP_DIR"

set -a
. <(sed 's/\r$//' ./.env)
set +a

DB_NAME="${POSTGRES_DB:-spring_code_passes}"
DB_USER="${POSTGRES_USER:-spring_code_bot}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="$BACKUP_DIR/${DB_NAME}_${STAMP}.sql.gz"

docker compose exec -T postgres pg_dump -U "$DB_USER" "$DB_NAME" | gzip -9 > "$OUT"

echo "Backup written: $OUT"
