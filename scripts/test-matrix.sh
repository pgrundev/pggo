#!/usr/bin/env bash
# Run unit tests, then the integration suite against every PostgreSQL version.
# Usage: scripts/test-matrix.sh [16 17 18 19beta1]
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/... ./cmd/...
status=0
while read -r v url; do
  echo "=== PostgreSQL $v"
  PGGO_TEST_URL="$url" PGGO_TEST_AUTH=1 go test -count=1 ./integration/ || status=1
done < <(scripts/pg-up.sh "$@")
exit $status
