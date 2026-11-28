#!/usr/bin/env bash
# Write load on the local cluster for autoscaling demos (M5): C parallel writers putting
# 1 KB items into table "users" for S seconds, then a summary of status codes.
#   KV_API_KEY=$(cat secrets/ops.key) scripts/local-load.sh [C=64] [S=480] [base-url]
set -euo pipefail
C=${1:-64}
S=${2:-480}
BASE=${3:-http://localhost:8080}
[ -n "${KV_API_KEY:-}" ] || { echo "set KV_API_KEY (the ops key)"; exit 1; }
CURL_HOME=$(mktemp -d)
OUT=$(mktemp)
export CURL_HOME BASE OUT
trap 'rm -rf "$CURL_HOME" "$OUT"' EXIT
printf 'header = "Authorization: Bearer %s"\n' "$KV_API_KEY" > "$CURL_HOME/.curlrc"
curl -s -o /dev/null -X POST -H 'Content-Type: application/json' \
  --data '{"name":"users","partitionKey":"userId"}' "$BASE/v1/tables"
VALUE=$(printf '{"pad":"%s"}' "$(head -c 1000 /dev/zero | tr '\0' x)")
export VALUE
END=$(($(date +%s) + S))
export END
writer() {
  local w=$1 i=0
  while [ "$(date +%s)" -lt "$END" ]; do
    curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H 'Content-Type: application/json' \
      --data "$VALUE" "$BASE/v1/tables/users/items/load-$w-$((i % 2000))" >> "$OUT"
    i=$((i + 1))
  done
}
export -f writer
seq 1 "$C" | xargs -P "$C" -I{} bash -c 'writer {}'
echo "load: $C writers for $S s: $(wc -l < "$OUT" | tr -d ' ') writes; by status: $(sort "$OUT" | uniq -c | tr '\n' ' ')"
