#!/usr/bin/env bash
# Run the agent test suite against PostgreSQL 18 in Docker.
# Requires the `claude` CLI. Env: AGENT_MODEL (default sonnet), AGENT_URL to override the DB.
# Extra args go to harness.py, e.g. --task find_user --runs 3
set -euo pipefail
cd "$(dirname "$0")/.."
make -s build
if [ -z "${AGENT_URL:-}" ]; then
  # A dedicated database, so the agent doesn't see integration-test tables.
  base=$(scripts/pg-up.sh 18 | awk '{print $2}')
  bin/pggo exec "$base" 'CREATE DATABASE pggo_agent' >/dev/null || true
  AGENT_URL=${base/\/pggo\?//pggo_agent?}
fi
url=$AGENT_URL
exec python3 agent_tests/harness.py --url "$url" --pggo bin/pggo "$@"
