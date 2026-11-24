#!/usr/bin/env bash
# Smoke test: the four operations against a running coordinator.
#   scripts/smoke.sh [base-url] [expected-version]   (default http://localhost:8080)
# Used locally after `docker compose up` and by the CI smoke job on the coordinator, which
# passes the release it deployed: then the coordinator and every registered shard must
# report exactly that version in /health. (Shard addresses are private: the version check
# runs only where they are reachable, i.e. on the coordinator, not on a laptop.)
# Exits non-zero at the first unexpected status.
set -euo pipefail
BASE=${1:-http://localhost:8080}
EXPECT_VERSION=${2:-}
TABLE=smoke
KEY="item-$(date +%s)-$RANDOM"   # a fresh key each run, so runs don't affect each other

# status METHOD PATH [BODY] -> prints the HTTP status code only
status() {
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$method" -H 'Content-Type: application/json' --data "$body" "$BASE$path"
  elif [ "$method" = HEAD ]; then
    # --head, not -X HEAD: with -X curl waits for a body a HEAD response never has.
    curl -s -o /dev/null -w '%{http_code}' --head "$BASE$path"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$method" "$BASE$path"
  fi
}

check() { # check DESCRIPTION EXPECTED ACTUAL
  if [ "$2" = "$3" ]; then echo "ok   $1 ($3)"; else echo "FAIL $1: got $3, want $2"; exit 1; fi
}

# Wait up to 60 s for the coordinator to see all its shards (it answers 503 until then).
for _ in $(seq 60); do
  curl -s "$BASE/health" | grep -q '"ready":true' && break
  sleep 1
done
curl -s "$BASE/health" | grep -q '"ready":true' || { echo "FAIL coordinator not ready"; exit 1; }

version_of() { curl -s "$1/health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }

if [ -n "$EXPECT_VERSION" ]; then
  check "coordinator version" "$EXPECT_VERSION" "$(version_of "$BASE")"
  # Every shard the coordinator knows, by the base URL it registered with.
  shards=$(curl -s "$BASE/internal/v1/shards" | grep -o '"address":"[^"]*"' | cut -d'"' -f4)
  [ -n "$shards" ] || { echo "FAIL no shards registered"; exit 1; }
  for addr in $shards; do
    check "shard $addr version" "$EXPECT_VERSION" "$(version_of "$addr")"
  done
fi

code=$(status POST /v1/tables "{\"name\":\"$TABLE\",\"partitionKey\":\"id\"}")
[ "$code" = 201 ] || [ "$code" = 409 ] || { echo "FAIL create table: $code"; exit 1; }
echo "ok   create table ($code)"

ITEM=/v1/tables/$TABLE/items/$KEY
check "CREATE"               200 "$(status PUT "$ITEM" '{"hello":"world"}')"
check "READ"                 200 "$(status GET "$ITEM")"
body=$(curl -s "$BASE$ITEM")
check "READ returns value"   '{"hello":"world"}' "$body"
check "EXISTS"               200 "$(status HEAD "$ITEM")"
check "DELETE"               204 "$(status DELETE "$ITEM")"
check "READ after delete"    404 "$(status GET "$ITEM")"
check "EXISTS after delete"  404 "$(status HEAD "$ITEM")"
echo "smoke test passed against $BASE"
