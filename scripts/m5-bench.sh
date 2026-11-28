#!/bin/bash
# M5 load profiles on the benchmark client (spec load-testing, ADR-0032), started through
# SSM by `make bench-profile`, like bench.sh by `make bench-aws`:
#
#   m5-bench.sh <bucket> <bench-version> <coordinator-url> <profile> <low> <high> [threads]
#
# YCSB workload A, throttled to a target rate (go-ycsb's `target`, ops/s over all threads):
#   calibrate  2 sets, autoscaling off: from <low> up in steps of <low>, 3 min each, to <high>
#   gradual    5-minute steps from <low> to <high> (4 equal steps), then <low> for 5 min
#   spike      <low> 5 min, <high> 15 min, <low> 10 min
#   sustained  <high> 30 min
# <low> is the M4 rate, <high> at least twice the rate at which calibration first crossed a
# scale-out threshold. Threads default to 96 (spec: 64-128).
#
# go-ycsb 1.0.3 has no time limit (`maxexecutiontime` is defined but unused), so each step
# runs target x seconds operations; if the store can't keep up the step runs longer, and
# `timeout` stops it at 1.5x its length (the summary then misses that step: the output says
# so). Each step prints its start time and go-ycsb's final summary: kept short because SSM
# returns at most 24,000 characters. Latency over time, replica sets and decisions come from
# the monitor (the dashboard, kvscale's log), keyed by these start times.
set -uo pipefail

BUCKET=$1 VERSION=$2 URL=$3 PROFILE=$4 LOW=$5 HIGH=$6 THREADS=${7:-96}
REGION=us-east-1
WORK=${KVBENCH_DIR:-/opt/kvbench}   # KVBENCH_DIR: a local trial run

mkdir -p "$WORK/out"
cd "$WORK" || exit 1
aws s3 cp --recursive "s3://$BUCKET/bench/$VERSION/" . --region "$REGION" --quiet || exit 1
chmod +x kvbench
( umask 077
  aws secretsmanager get-secret-value --secret-id velmakina-kv/ops-key --query SecretString \
    --output text --region "$REGION" 2>/dev/null | sed 's/^/kv.apikey=/' > key.properties ) || true
COMMON=(-P workloads/common -P key.properties -p "kv.url=$URL" -p "threadcount=$THREADS")

# step <rate> <seconds>: one throttled run of workload A.
n=0
step() {
  local rate=$1 secs=$2; n=$((n+1))
  local f="out/$PROFILE-step$n.txt"
  echo "== step $n: $rate ops/s for $secs s, start $(date -u +%FT%TZ)"
  timeout -s INT $((secs * 3 / 2)) ./kvbench run "${COMMON[@]}" -P workloads/workloada \
    -p "target=$rate" -p "operationcount=$((rate * secs))" > "$f" 2>&1
  local rc=$?
  [ $rc -eq 124 ] && echo "   stopped by timeout at $((secs * 3 / 2)) s: the store did not keep up with $rate ops/s"
  [ $rc -ne 0 ] && [ $rc -ne 124 ] && { echo "   FAILED (exit $rc):"; tail -n 15 "$f"; }
  sed -n '/finished, takes/,$p' "$f" | grep -E 'finished|^(READ|UPDATE|TOTAL)'
  grep -c 'request failed' "$f" | sed 's/^/   failed requests logged: /'
}

echo "== profile $PROFILE, low $LOW, high $HIGH ops/s, $THREADS threads, url $URL"
echo "== load ($(date -u +%FT%TZ))"
./kvbench load "${COMMON[@]}" -P workloads/workloada > out/load.txt 2>&1 || { tail -n 15 out/load.txt; exit 1; }

case $PROFILE in
  calibrate) for ((r = LOW; r <= HIGH; r += LOW)); do step "$r" 180; done ;;
  gradual)   d=$(( (HIGH - LOW) / 4 ))
             for i in 0 1 2 3 4; do step $((LOW + i * d)) 300; done; step "$LOW" 300 ;;
  spike)     step "$LOW" 300; step "$HIGH" 900; step "$LOW" 600 ;;
  sustained) step "$HIGH" 1800 ;;
  *) echo "unknown profile $PROFILE (calibrate, gradual, spike, sustained)"; exit 2 ;;
esac
echo "== done ($(date -u +%FT%TZ))"
