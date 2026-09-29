#!/usr/bin/env bash
# Remove the containers started by pg-up.sh.
docker ps -a --format '{{.Names}}' | grep '^pggo-test-pg' | xargs -r docker rm -f >/dev/null
