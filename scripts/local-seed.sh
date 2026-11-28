#!/usr/bin/env bash
# Put N items into the local cluster's table "users" (M5 task 5.5: data to migrate).
#   KV_API_KEY=$(cat secrets/ops.key) scripts/local-seed.sh [N] [base-url]   (default 20000)
# 16 PUTs in parallel; items are seed-0 .. seed-<N-1> with value {"n": i}. Exits non-zero
# if any PUT doesn't answer 200.
set -euo pipefail
N=${1:-20000}
BASE=${2:-http://localhost:8080}
[ -n "${KV_API_KEY:-}" ] || { echo "set KV_API_KEY (the ops key)"; exit 1; }
CURL_HOME=$(mktemp -d)
export CURL_HOME BASE
trap 'rm -rf "$CURL_HOME"' EXIT
printf 'header = "Authorization: Bearer %s"\n' "$KV_API_KEY" > "$CURL_HOME/.curlrc"

curl -s -o /dev/null -X POST -H 'Content-Type: application/json' \
  --data '{"name":"users","partitionKey":"userId"}' "$BASE/v1/tables" # 201, or 409 if it exists

start=$(date +%s)
failed=$(seq 0 $((N - 1)) | xargs -P 16 -I{} sh -c \
  'c=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "Content-Type: application/json" --data "{\"n\":{}}" "$BASE/v1/tables/users/items/seed-{}"); [ "$c" = 200 ] || echo "seed-{} $c"' | tee /dev/stderr | wc -l)
echo "seeded $N items in $(( $(date +%s) - start )) s; $failed failed"
[ "$failed" -eq 0 ]
