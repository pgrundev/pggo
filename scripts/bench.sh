#!/usr/bin/env bash
# Benchmark pggo vs psql vs a minimal pgx program against PostgreSQL 18 in Docker.
# Writes benchmarks/results/<date>.md. Override the target with BENCH_URL.
set -euo pipefail
cd "$(dirname "$0")/.."
make -s build
(cd benchmarks && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../bin/pgxmin ./pgxmin)
url=${BENCH_URL:-$(scripts/pg-up.sh 18 | awk '{print $2}')}
mkdir -p benchmarks/results
out=benchmarks/results/$(date -u +%Y%m%dT%H%M%SZ).md
(cd benchmarks && go run ./runner -url "$url" -n "${BENCH_N:-300}" -warm "${BENCH_WARM:-5000}") | tee "$out"
echo "wrote $out" >&2
