#!/bin/sh
set -eu

DB_PATH="${DB_PATH:-/data/moesekai.db}"
DATA_DIR="${DATA_DIR:-/data}"
SEED_DIR="/app/seed-translations"

# Service ports (internal)
WEB_PORT="${WEB_PORT:-3000}"
BACKEND_PORT="${BACKEND_PORT:-9090}"

echo "=== MOESEKAI v2 STARTUP ==="
echo "DB_PATH:      $DB_PATH"
echo "DATA_DIR:     $DATA_DIR"
echo "WEB_PORT:     $WEB_PORT (Next.js, internal)"
echo "BACKEND_PORT: $BACKEND_PORT (Go API, internal)"
echo "NGINX:        :80 (public entry point)"

mkdir -p "$DATA_DIR"

# On first run (no DB yet), seed translations from the image into SQLite.
if [ ! -f "$DB_PATH" ] && [ -d "$SEED_DIR" ]; then
  echo "No database found; migrating seed translations into SQLite..."
  ./moesekai-migrate -src "$SEED_DIR" -db "$DB_PATH" -verify=false || {
    echo "WARNING: seed migration failed; starting with empty database"
  }
else
  echo "Database present or no seed; skipping migration."
fi

# Start the Next.js console in the background. Output goes straight to the
# container's stdout (inherited from PID 1) so `docker logs` shows it.
echo "Starting console (Next.js) on :${WEB_PORT}..."
( cd /app/web && PORT="${WEB_PORT}" BACKEND_ORIGIN="http://localhost:${BACKEND_PORT}" npx next start -p "${WEB_PORT}" ) &

# Start the Go backend in the background. Its log lines are already tagged
# ([http], [translate], [backup], ...) and timestamped.
echo "Starting backend (Go) on :${BACKEND_PORT}..."
( PORT="${BACKEND_PORT}" ./moesekai-server ) &

# Start nginx as the foreground process (reverse proxy on port 80).
# Its access/error logs go to /dev/stdout and /dev/stderr (see nginx.conf),
# so all three services' logs appear together in `docker logs`.
echo "Starting nginx reverse proxy on :80..."
exec nginx -g 'daemon off;'
