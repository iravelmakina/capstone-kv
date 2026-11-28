#!/usr/bin/env bash
# M5 task 5.5: shard-3 joins (join) or leaves (leave) the local cluster through a migration
# (ADR-0033) while a verifier writes. Prints keys and log bytes per replica set before and
# after (and requests per second per shard if the local Prometheus runs), and checks that
# every acknowledged write reads back with its value.
#   KV_API_KEY=$(cat secrets/ops.key) scripts/local-migrate.sh join|leave
# join starts shard-3's containers (Compose profile shard3) and waits for its leader;
# leave stops them after the migration and has the coordinator forget the retired set.
set -euo pipefail
ACTION=${1:?usage: local-migrate.sh join|leave}
BASE=${BASE:-http://localhost:8080}
PROM=${PROM:-http://localhost:9090}
[ -n "${KV_API_KEY:-}" ] || { echo "set KV_API_KEY (the ops key)"; exit 1; }
CURL_HOME=$(mktemp -d)
WORK=$(mktemp -d)
export CURL_HOME
VPID=
trap '[ -n "$VPID" ] && kill "$VPID" 2>/dev/null; rm -rf "$CURL_HOME" "$WORK"' EXIT
printf 'header = "Authorization: Bearer %s"\n' "$KV_API_KEY" > "$CURL_HOME/.curlrc"

port_of() { # replica ID -> its health port on this machine (docker-compose.yml)
  case $1 in
    1a) echo 8111 ;; 1b) echo 8112 ;; 1c) echo 8113 ;;
    2a) echo 8121 ;; 2b) echo 8122 ;; 2c) echo 8123 ;;
    3a) echo 8131 ;; 3b) echo 8132 ;; 3c) echo 8133 ;;
  esac
}

cluster() { curl -s "$BASE/v1/admin/cluster"; }

sizes() { # one line per set: state, leader, keys and log bytes from the leader's /health
  cluster | jq -r '.sets[] | [.id, .state, .leader] | @tsv' | while IFS=$'\t' read -r id state leader; do
    h=$(curl -s "http://127.0.0.1:$(port_of "$leader")/health" 2>/dev/null || echo '{}')
    printf '  %-8s %-8s leader %-3s keys %7s   log %11s bytes\n' "$id" "$state" "$leader" \
      "$(jq -r '.keys // "-"' <<<"$h")" "$(jq -r '.logBytes // "-"' <<<"$h")"
  done
  if curl -sf "$PROM/-/ready" >/dev/null 2>&1; then
    curl -s "$PROM/api/v1/query" --data-urlencode 'query=sum by (shard) (rate(kv_requests_total{shard!=""}[1m]))' |
      jq -r '.data.result[] | "  requests/s \(.metric.shard): \(.value[1] | tonumber | floor)"'
  fi
}

wait_for() { # wait_for DESCRIPTION SECONDS JQ-CONDITION-ON-THE-CLUSTER-STATE
  for _ in $(seq "$2"); do
    [ "$(cluster | jq -r "$3")" = true ] && return 0
    sleep 1
  done
  echo "FAIL: $1 within $2 s"
  cluster | jq '.migration'
  exit 1
}

verifier() { # writes verify-<run>-<i> until killed; logs "status key value" per write
  local i=0 k v code
  while :; do
    k="verify-$RUN-$i"
    v="{\"i\":$i}"
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' \
      --data "$v" "$BASE/v1/tables/users/items/$k")
    echo "$code $k $v" >> "$WORK/writes"
    i=$((i + 1))
  done
}

curl -s -o /dev/null -X POST -H 'Content-Type: application/json' \
  --data '{"name":"users","partitionKey":"userId"}' "$BASE/v1/tables" # 201, or 409 if it exists
RUN=$(date +%s)

case $ACTION in
  join)
    docker compose --profile shard3 up -d r3a r3b r3c
    echo "waiting for shard-3 to register and choose its leader"
    wait_for "shard-3 pending and serving (confirmed leader, quorum)" 240 \
      '[.sets[] | select(.id == "shard-3" and .state == "pending" and .serving)] | length == 1'
    body='{"add":"shard-3"}'
    ;;
  leave)
    body='{"remove":"shard-3"}'
    ;;
  *)
    echo "usage: local-migrate.sh join|leave"
    exit 2
    ;;
esac

echo "== before"
sizes
verifier &
VPID=$!
sleep 1
start=$(date +%s)
resp=$(curl -s -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' --data "$body" "$BASE/v1/admin/migrations")
[ "$(tail -1 <<<"$resp")" = 201 ] || { echo "FAIL: start: $resp"; exit 1; }
echo "migration $(head -1 <<<"$resp" | jq -r .id) started"
wait_for "the migration to finish" 900 '.migration.phase == "done" or .migration.phase == "aborted"'
took=$(($(date +%s) - start))
phase=$(cluster | jq -r .migration.phase)
sleep 1
kill "$VPID"
wait "$VPID" 2>/dev/null || true
VPID=
echo "== after: $phase in $took s"
sizes
[ "$phase" = done ] || { echo "FAIL: migration $phase"; exit 1; }

total=$(wc -l < "$WORK/writes" | tr -d ' ')
acked=$(grep -c '^200 ' "$WORK/writes" || true)
fivexx=$(grep -c '^5' "$WORK/writes" || true)
bad=0
while read -r code k v; do
  [ "$code" = 200 ] || continue
  got=$(curl -s "$BASE/v1/tables/users/items/$k")
  if [ "$got" != "$v" ]; then
    echo "FAIL: $k reads $got, want $v"
    bad=$((bad + 1))
  fi
done < "$WORK/writes"
echo "verifier: $total writes during the migration, $acked acknowledged, $fivexx answered 5xx, $bad acknowledged but not read back"

if [ "$ACTION" = leave ]; then
  docker compose --profile shard3 stop r3a r3b r3c
  echo "forget the retired shard-3: $(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/internal/v1/shards/shard-3")"
fi
[ "$bad" -eq 0 ] && [ "$fivexx" -eq 0 ]
