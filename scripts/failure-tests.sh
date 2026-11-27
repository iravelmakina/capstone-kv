#!/usr/bin/env bash
# Failure tests on the local cluster (M3 task 5.2): one block per scenario of the
# `replication` and `quorum-reads` specs, run against real containers.
#   docker compose down -v && make local-up && KV_API_KEY=$(cat secrets/ops.key) scripts/failure-tests.sh
# Needs a fresh cluster (shard-1 led by 1a, shard-2 by 2b, epoch 1), docker, curl and jq.
# Prints what it does and checks every result; exits non-zero at the first failure.
set -euo pipefail
# API key (ADR-0027) from KV_API_KEY: put in a temporary curl config, so every curl below
# sends it and the key never appears on a command line.
if [ -n "${KV_API_KEY:-}" ]; then
  CURL_HOME=$(mktemp -d)
  export CURL_HOME
  trap 'rm -rf "$CURL_HOME"' EXIT
  printf 'header = "Authorization: Bearer %s"\n' "$KV_API_KEY" > "$CURL_HOME/.curlrc"
fi
BASE=http://localhost:8080
TABLE=ft

port() { # replica ID -> its port on 127.0.0.1 (docker-compose.yml)
  case $1 in 1a) echo 8111 ;; 1b) echo 8112 ;; 1c) echo 8113 ;; 2a) echo 8121 ;; 2b) echo 8122 ;; 2c) echo 8123 ;; esac
}
health() { curl -s --max-time 2 "http://127.0.0.1:$(port "$1")/health"; }
last()   { health "$1" | jq -r '.last.lsn'; }
reads()  { health "$1" | jq -r '.reads'; }
shards() { curl -s "$BASE/internal/v1/shards"; }
leader() { shards | jq -r --arg s "$1" '.[] | select(.id==$s) | .leader'; }
epoch()  { shards | jq -r --arg s "$1" '.[] | select(.id==$s) | .epoch'; }
status() { # METHOD PATH [BODY] -> HTTP status
  if [ -n "${3:-}" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$1" -H 'Content-Type: application/json' --data "$3" "$BASE$2"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$1" "$BASE$2"
  fi
}
item() { echo "/v1/tables/$TABLE/items/$1"; }
section() { printf '\n===== [%s] %s =====\n' "$1" "$2"; }
check() { # DESCRIPTION EXPECTED ACTUAL
  if [ "$2" = "$3" ]; then echo "ok   $1 ($3)"; else echo "FAIL $1: got $3, want $2"; exit 1; fi
}
wait_for() { # DESCRIPTION SECONDS COMMAND...: retry COMMAND every second until it succeeds
  local what=$1 secs=$2; shift 2
  for _ in $(seq "$secs"); do "$@" >/dev/null 2>&1 && { echo "ok   $what"; return; }; sleep 1; done
  echo "FAIL $what (after ${secs}s)"; exit 1
}
same_last() { [ "$(last "$1")" = "$(last "$2")" ]; }
dc() { echo "\$ docker compose $*"; docker compose "$@" >/dev/null 2>&1; }

section M3-L1 "cluster ready: two replica sets, first leaders 1a and 2b, epoch 1"
wait_for "coordinator ready" 60 sh -c "curl -s $BASE/health | grep -q '\"ready\":true'"
shards | jq -c '.[] | {id, epoch, leader, live: [.replicas[] | select(.available) | .id]}'
check "shard-1 leader" 1a "$(leader shard-1)"
check "shard-2 leader" 2b "$(leader shard-2)"
status POST /v1/tables "{\"name\":\"$TABLE\",\"partitionKey\":\"id\"}" >/dev/null

section M3-L2 "200 puts reach every replica (spec: Follower stays current)"
S1=() S2=()
for i in $(seq 200); do
  before=$(last 1a)
  check_code=$(status PUT "$(item k$i)" "{\"i\":$i}")
  [ "$check_code" = 200 ] || { echo "FAIL put k$i: $check_code"; exit 1; }
  if [ "$(last 1a)" != "$before" ]; then S1+=("k$i"); else S2+=("k$i"); fi
done
echo "ok   200 puts (shard-1 got ${#S1[@]}, shard-2 got ${#S2[@]})"
wait_for "1b and 1c at the leader's LSN" 5 sh -c "[ $(last 1a) = \$(curl -s 127.0.0.1:8112/health | jq .last.lsn) ] && [ $(last 1a) = \$(curl -s 127.0.0.1:8113/health | jq .last.lsn) ]"
wait_for "2a and 2c at the leader's LSN" 5 sh -c "[ $(last 2b) = \$(curl -s 127.0.0.1:8121/health | jq .last.lsn) ] && [ $(last 2b) = \$(curl -s 127.0.0.1:8123/health | jq .last.lsn) ]"
for r in 1a 1b 1c 2a 2b 2c; do health "$r" | jq -c '{replica, role, epoch, last: .last.lsn, lag, keys}'; done

section M3-L3 "reads spread over all replicas (spec: Balanced reads, R = 2)"
K=${S1[0]}
b1a=$(reads 1a) b1b=$(reads 1b) b1c=$(reads 1c)
for _ in $(seq 3000); do curl -s -o /dev/null "$BASE$(item "$K")"; done
d1a=$(( $(reads 1a) - b1a )) d1b=$(( $(reads 1b) - b1b )) d1c=$(( $(reads 1c) - b1c ))
echo "3000 reads of $K -> replica reads: 1a=$d1a 1b=$d1b 1c=$d1c (want 1800-2200 each)"
for d in $d1a $d1b $d1c; do [ "$d" -ge 1800 ] && [ "$d" -le 2200 ] || { echo "FAIL unbalanced"; exit 1; }; done
echo "ok   balanced"

section M3-L4 "a follower stops and restarts (specs: One follower down, Follower restarts behind)"
dc stop r1b
check "put with 1b down" 200 "$(status PUT "$(item l4-a)" '{"v":1}')"
for k in "${S1[@]:0:5}"; do check "read $k with 1b down" 200 "$(status GET "$(item "$k")")"; done
for i in $(seq 50); do status PUT "$(item "${S1[$((i % ${#S1[@]}))]}")" "{\"round\":2,\"i\":$i}" >/dev/null; done
echo "leader 1a now at LSN $(last 1a); 1b stopped behind it"
dc start r1b
wait_for "1b caught up with the leader" 15 same_last 1a 1b
docker compose logs r1b 2>/dev/null | grep -E "replayed up to LSN" | tail -1 | sed 's/^/     /'
health 1b | jq -c '{replica, role, last: .last.lsn, lag}'

section M3-L5 "both followers down (spec: Both followers down; Two replicas down)"
K504=${S1[1]}                       # a shard-1 key with a committed value
committed=$(curl -s "$BASE$(item "$K504")")
echo "$K504 holds $committed"
dc stop r1b r1c
t0=$(date +%s)
code=$(status PUT "$(item "$K504")" '{"never":"committed"}')
echo "put took $(( $(date +%s) - t0 ))s"
check "put with both followers down" 504 "$code"
check "read with two replicas down" 503 "$(status GET "$(item "${S1[0]}")")"
curl -s "$BASE/health" | jq -c '.failures'
on_leader=$(curl -s "127.0.0.1:8111/internal/v1/kv/$(printf 'ft\0%s\0' "$K504" | base64 | tr '+/' '-_' | tr -d '=')")
check "the leader still shows the committed value" "$committed" "$on_leader"

section M3-L6 "leader replaced; the former leader drops its uncommitted write (specs: Leader replaced, Former leader returns)"
dc stop r1a
dc start r1b r1c
echo "waiting for 1a to be detected as dead (15 s) and a follower promoted..."
wait_for "shard-1 promoted to epoch 2" 40 sh -c "[ \$(curl -s $BASE/internal/v1/shards | jq '.[] | select(.id==\"shard-1\") | .epoch') = 2 ]"
new=$(leader shard-1)
echo "new leader of shard-1: $new"
wait_for "put on the new leader" 15 sh -c "[ \$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' --data '{\"v\":2}' $BASE$(item l6-new)) = 200 ]"
for k in "${S1[@]:0:5}"; do check "committed $k survived" 200 "$(status GET "$(item "$k")")"; done
check "the 504 put never appeared" "$committed" "$(curl -s "$BASE$(item "$K504")")"
dc start r1a
wait_for "1a back as a follower at the new leader's LSN" 20 sh -c "[ \$(curl -s 127.0.0.1:8111/health | jq -r .role) = follower ] && [ \$(curl -s 127.0.0.1:8111/health | jq .last.lsn) = \$(curl -s 127.0.0.1:$(port "$new")/health | jq .last.lsn) ]"
docker compose logs r1a 2>/dev/null | grep -E "cutting back" | tail -1 | sed 's/^/     /'
health 1a | jq -c '{replica, role, epoch, leader, last}'

section M3-L7 "leader and one follower down: no promotion (spec: Only one follower live)"
dc stop r2b r2c
sleep 20
check "shard-2 still epoch 1" 1 "$(epoch shard-2)"
check "shard-2 leader unchanged" 2b "$(leader shard-2)"
check "put to shard-2" 503 "$(status PUT "$(item "${S2[0]}")" '{"v":3}')"
check "read from shard-2 (1 replica live)" 503 "$(status GET "$(item "${S2[0]}")")"
check "shard-1 unaffected" 200 "$(status GET "$(item "${S1[0]}")")"
dc start r2b r2c
wait_for "shard-2 writable again" 30 sh -c "[ \$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' --data '{\"v\":4}' $BASE$(item "${S2[0]}")) = 200 ]"

section M3-L8 "coordinator restart keeps leaders and epochs (spec: Coordinator restart keeps the leader)"
before=$(shards | jq -c '[.[] | {id, epoch, leader}]')
dc restart coordinator
wait_for "coordinator ready again" 60 sh -c "curl -s $BASE/health | grep -q '\"ready\":true'"
after=$(shards | jq -c '[.[] | {id, epoch, leader}]')
echo "before: $before"
echo "after:  $after"
check "same leaders and epochs" "$before" "$after"

section M3-L9 "rebuild a replica from the leader (spec: Rebuild)"
echo "\$ make rebuild-replica R=1c"
make rebuild-replica R=1c >/dev/null 2>&1
wait_for "1c rebuilt and caught up" 30 same_last "$new" 1c
docker compose logs r1c 2>/dev/null | grep -E "replayed up to LSN" | tail -1 | sed 's/^/     /'
health 1c | jq -c '{replica, role, last: .last.lsn, keys}'

section M4-L11 "a late retry loses to a newer write (spec: Late retry loses)"
# header NAME: prints one response header of the request that follows (curl args).
header() { local name=$1; shift; curl -s -D - -o /dev/null "$@" | tr -d '\r' | awk -F': ' -v n="$name" 'tolower($1)==tolower(n) {print $2}'; }
KL=${S2[1]}                         # a shard-2 key; shard-2 is still led by 2b
dc stop r2a r2c
t1=$(header X-KV-Timestamp -X PUT -H 'Content-Type: application/json' --data '{"by":"A"}' "$BASE$(item "$KL")")
check "A's put timed out with a stamp" yes "$([ -n "$t1" ] && echo yes)"
dc start r2a r2c
wait_for "shard-2 writable again" 30 sh -c "[ \$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' --data '{\"by\":\"B\"}' $BASE$(item "$KL")) = 200 ]"
check "A's retry with its first stamp is superseded" true \
  "$(header X-KV-Superseded -X PUT -H 'Content-Type: application/json' -H "X-KV-Timestamp: $t1" --data '{"by":"A"}' "$BASE$(item "$KL")")"
check "reads return B's value" '{"by":"B"}' "$(curl -s "$BASE$(item "$KL")")"

section M3-L10 "smoke test after all of it"
scripts/smoke.sh "$BASE" | tail -1
echo
echo "all failure tests passed"
