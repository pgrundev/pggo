#!/usr/bin/env bash
# Run unit tests, then the integration suite against every PostgreSQL version.
# Usage: scripts/test-matrix.sh [16 17 18 19beta1]
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/... ./cmd/...
status=0
while read -r v url; do
  echo "=== PostgreSQL $v"
  cert=$(mktemp -t pggo-root.XXXXXX)
  docker exec "pggo-test-pg$v" cat /etc/ssl/certs/ssl-cert-snakeoil.pem > "$cert"
  PGGO_TEST_URL="$url" PGGO_TEST_AUTH=1 PGGO_TEST_SSLROOTCERT="$cert" go test -count=1 ./integration/ || status=1
  rm -f "$cert"
done < <(scripts/pg-up.sh "$@")
exit $status
