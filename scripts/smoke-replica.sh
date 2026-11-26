#!/usr/bin/env bash
# Smoke test with one follower down (M3 task 7.1; spec replication, "One follower down").
# Talks to the coordinator only; the caller stops and starts the replicas in between:
#   scripts/smoke-replica.sh BASE pick        prints a host that runs no shard leader
#   scripts/smoke-replica.sh BASE down HOST   waits until the coordinator sees every replica on
#                                             HOST as down, then puts keys and reads them back
#   scripts/smoke-replica.sh BASE up HOST     waits until every replica on HOST is back and has
#                                             caught up with its leader
# In AWS (`make smoke-replica`, on the coordinator through SSM) HOST is a shard node's private
# IP; a node holds one replica of each shard (ADR-0020), so stopping it stops one follower of
# each shard. Locally (docker compose) each replica is its own host, so HOST is one replica.
# Exits non-zero at the first unexpected result.
set -euo pipefail
BASE=$1
PHASE=$2
HOST=${3:-}
TABLE=smoke
KEYS=10   # ten random keys: all in one shard with probability 2^-9

fail() { echo "FAIL $*"; exit 1; }

# The coordinator's view of the replica sets (GET /internal/v1/shards), asked one question:
#   pick              a host with no leader on it (first in sort order), or nothing
#   leaders HOST      the shards whose leader is on HOST
#   down HOST         yes if every replica on HOST is reported unavailable
#   caught-up HOST    yes if every replica on HOST is available and at its leader's last record
ask() {
  curl -s -m 5 "$BASE/internal/v1/shards" | python3 -c '
import json, sys
from urllib.parse import urlparse
what, host = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else "")
sets = json.load(sys.stdin)
def host_of(r): return urlparse(r["address"]).hostname
def leader(s): return next(r for r in s["replicas"] if r["id"] == s["leader"])
on = [(s, r) for s in sets for r in s["replicas"] if host_of(r) == host]
if what == "pick":
    hosts = {host_of(r) for s in sets for r in s["replicas"]} - {host_of(leader(s)) for s in sets}
    print(sorted(hosts)[0] if hosts else "")
elif what == "leaders":
    print(" ".join(s["id"] for s in sets if host_of(leader(s)) == host))
elif what == "down":
    print("yes" if on and not any(r["available"] for _, r in on) else "no")
elif what == "caught-up":
    print("yes" if on and all(r["available"] and r["last"] == leader(s)["last"] for s, r in on) else "no")
' "$@"
}

case $PHASE in
  pick)
    host=$(ask pick)
    [ -n "$host" ] || fail "every host runs a leader"
    echo "$host"
    ;;

  down)
    [ -n "$HOST" ] || fail "usage: $0 BASE down HOST"
    # Checked first: a stopped leader is replaced after 15 s, and then the check would pass.
    leaders=$(ask leaders "$HOST")
    [ -z "$leaders" ] || fail "$HOST runs the leader of $leaders: not a follower-only host"
    # 3 missed heartbeats (15 s) mark a replica down; the caller has already stopped it.
    for _ in $(seq 45); do [ "$(ask down "$HOST")" = yes ] && break; sleep 1; done
    [ "$(ask down "$HOST")" = yes ] || fail "replicas on $HOST still reported live after 45 s"
    echo "ok   every replica on $HOST reported down"

    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
      --data "{\"name\":\"$TABLE\",\"partitionKey\":\"id\"}" "$BASE/v1/tables")
    [ "$code" = 201 ] || [ "$code" = 409 ] || fail "create table: $code"

    run="replica-$(date +%s)-$RANDOM"
    for i in $(seq "$KEYS"); do
      item=$BASE/v1/tables/$TABLE/items/$run-$i
      code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' \
        --data "{\"n\":$i}" "$item")
      [ "$code" = 200 ] || fail "PUT $run-$i: $code"
      body=$(curl -s "$item")
      [ "$body" = "{\"n\":$i}" ] || fail "GET $run-$i: $body"
      code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$item")
      [ "$code" = 204 ] || fail "DELETE $run-$i: $code"
    done
    echo "ok   $KEYS puts committed and read back, then deleted, with $HOST down"
    ;;

  up)
    [ -n "$HOST" ] || fail "usage: $0 BASE up HOST"
    # The LSNs come from heartbeats (every 5 s), so a returning replica and its leader can
    # briefly show the same stale number. Nothing writes now, so "caught up" counts only
    # once it has held for 6 s in a row: by then both have reported again.
    held=0
    for _ in $(seq 120); do
      if [ "$(ask caught-up "$HOST")" = yes ]; then held=$((held + 1)); else held=0; fi
      [ "$held" -ge 6 ] && break
      sleep 1
    done
    [ "$held" -ge 6 ] || fail "replicas on $HOST not back and caught up after 120 s"
    echo "ok   every replica on $HOST back and at its leader's last record"
    ;;

  *)
    fail "usage: $0 BASE pick | down HOST | up HOST"
    ;;
esac
