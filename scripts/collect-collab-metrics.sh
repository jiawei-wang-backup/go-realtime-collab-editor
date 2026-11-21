#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${ADMIN_A_URL:?set ADMIN_A_URL, for example http://127.0.0.1:19090}"
: "${ADMIN_B_URL:?set ADMIN_B_URL, for example http://127.0.0.1:19091}"
: "${NODE_A_PID:?set NODE_A_PID for this fixture}"
: "${NODE_B_PID:?set NODE_B_PID for this fixture}"
OUTPUT_DIR=${OUTPUT_DIR:-"${TMPDIR:-/tmp}/collab-metrics-$(date -u +%Y%m%dT%H%M%SZ)"}
INTERVAL_SECONDS=${INTERVAL_SECONDS:-5}

[[ "$NODE_A_PID" =~ ^[0-9]+$ && "$NODE_B_PID" =~ ^[0-9]+$ ]] || {
  printf 'NODE_A_PID and NODE_B_PID must be numeric\n' >&2
  exit 64
}
[[ "$INTERVAL_SECONDS" =~ ^[1-9][0-9]*$ ]] || {
  printf 'INTERVAL_SECONDS must be a positive integer\n' >&2
  exit 64
}
mkdir -p -- "$OUTPUT_DIR"

printf 'Metrics directory: %s\n' "$OUTPUT_DIR"
printf 'Sampling every %ss; press Ctrl+C to stop.\n' "$INTERVAL_SECONDS"

while :; do
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  curl -fsS --max-time 3 "$ADMIN_A_URL/metrics" >"$OUTPUT_DIR/node-a-$stamp.prom"
  curl -fsS --max-time 3 "$ADMIN_B_URL/metrics" >"$OUTPUT_DIR/node-b-$stamp.prom"
  ps -o pid=,ppid=,%cpu=,rss=,etime= -p "$NODE_A_PID,$NODE_B_PID" >"$OUTPUT_DIR/process-$stamp.txt" || true
  printf '%s sampled\n' "$stamp"
  sleep "$INTERVAL_SECONDS"
done
