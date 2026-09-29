#!/usr/bin/env bash
# Start throwaway PostgreSQL containers for the integration matrix.
# Usage: scripts/pg-up.sh [16 17 18 19beta1]
# Prints one "VERSION URL" line per server.
set -euo pipefail
versions=("$@")
[ $# -eq 0 ] && versions=(16 17 18 19beta1)
for v in "${versions[@]}"; do
  major=${v%%[!0-9]*}
  port=$((55400 + major))
  name="pggo-test-pg$v"
  if ! docker ps --format '{{.Names}}' | grep -qx "$name"; then
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --name "$name" -p "127.0.0.1:$port:5432" \
      -e POSTGRES_PASSWORD=pggo -e POSTGRES_USER=pggo -e POSTGRES_DB=pggo \
      "postgres:$v" \
      -c ssl=on -c ssl_cert_file=/etc/ssl/certs/ssl-cert-snakeoil.pem \
      -c ssl_key_file=/etc/ssl/private/ssl-cert-snakeoil.key >/dev/null
  fi
  # pg_isready can succeed against the entrypoint's temporary init server,
  # so wait until the final server accepts TCP queries.
  for _ in $(seq 1 120); do
    docker exec "$name" psql -h 127.0.0.1 -U pggo -d pggo -tAc 'select 1' >/dev/null 2>&1 &&
      docker logs "$name" 2>&1 | grep -q 'PostgreSQL init process complete' && break
    sleep 0.5
  done
  # md5 and cleartext auth for two dedicated roles (TestAuthMethods).
  docker exec "$name" bash -c 'f=/var/lib/postgresql/data/pg_hba.conf; [ -f "$f" ] || f=$(ls /var/lib/postgresql/*/docker/pg_hba.conf 2>/dev/null | head -1);
    grep -q md5user "$f" || { sed -i "1i host all md5user all md5\nhost all cleartextuser all password" "$f"; psql -U pggo -d pggo -qc "select pg_reload_conf()" >/dev/null; }'
  echo "$v postgres://pggo:pggo@127.0.0.1:$port/pggo?sslmode=disable"
done
