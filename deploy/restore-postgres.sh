#!/usr/bin/env bash
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "Usage: bash deploy/restore-postgres.sh path/to/backup.sql.gz" >&2
  exit 1
fi

cd "$(dirname "$0")/.."

BACKUP_FILE="$1"
if [ ! -f "$BACKUP_FILE" ]; then
  echo "Backup file not found: $BACKUP_FILE" >&2
  exit 1
fi

set -a
. <(sed 's/\r$//' ./.env)
set +a

DB_NAME="${POSTGRES_DB:-spring_code_passes}"
DB_USER="${POSTGRES_USER:-spring_code_bot}"

echo "This will drop and recreate database '$DB_NAME'."
read -r -p "Type RESTORE to continue: " CONFIRM
if [ "$CONFIRM" != "RESTORE" ]; then
  echo "Cancelled."
  exit 1
fi

docker compose stop bot
docker compose exec -T postgres psql -U "$DB_USER" postgres -v ON_ERROR_STOP=1 <<SQL
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${DB_NAME}';
DROP DATABASE IF EXISTS "${DB_NAME}";
CREATE DATABASE "${DB_NAME}" OWNER "${DB_USER}";
SQL

gzip -dc "$BACKUP_FILE" | docker compose exec -T postgres psql -U "$DB_USER" "$DB_NAME" -v ON_ERROR_STOP=1
docker compose up -d bot

echo "Restore completed."
