#!/bin/bash
# Benchmark run on the client instance (ADR-0016), started through SSM by `make bench-aws`:
#
#   bench.sh <bucket> <bench-version> <coordinator-url> [runs] [workloads]
#
# Downloads kvbench and the workload files from s3://<bucket>/bench/<bench-version>/, loads
# the records once, runs each workload (default "a b") <runs> times (default 3), and prints
# each run's final summary. M3's variants (W = 1, a follower down) run only "a". The instance role can read S3 but not write it (M1 evidence 63), so
# results leave through the SSM command output; `make bench-aws` saves them locally and
# uploads them to S3 with its own credentials. Kept short: SSM returns at most 24,000
# characters, so go-ycsb's progress lines (every 10 s) stay on the instance.
set -uo pipefail

BUCKET=$1 VERSION=$2 URL=$3 RUNS=${4:-3} WORKLOADS=${5:-a b}
REGION=us-east-1
WORK=/opt/kvbench

mkdir -p "$WORK/out"
cd "$WORK" || exit 1
aws s3 cp --recursive "s3://$BUCKET/bench/$VERSION/" . --region "$REGION" --quiet || exit 1
chmod +x kvbench

# The same properties for every run; per-run differences come only from the workload file.
COMMON=(-P workloads/common -p "kv.url=$URL")

# step <output file> <kvbench args...>: run; print the final summary (everything from the
# "finished" line on), or on failure the output's last lines, and stop.
step() {
  local f=$1; shift
  ./kvbench "$@" > "out/$f" 2>&1
  local rc=$?
  if [ $rc -ne 0 ]; then
    echo "FAILED (exit $rc): kvbench $* -- last lines of $f:"
    tail -n 25 "out/$f"
    exit $rc
  fi
  sed -n '/finished, takes/,$p' "out/$f"
  grep -c 'request failed' "out/$f" | sed 's/^/failed requests logged: /'
}

echo "== load ($(date -u +%T))"
step load.txt load "${COMMON[@]}" -P workloads/workloada

for w in $WORKLOADS; do
  for i in $(seq "$RUNS"); do
    echo "== workload $w, run $i ($(date -u +%T))"
    step "workload$w-run$i.txt" run "${COMMON[@]}" -P "workloads/workload$w"
  done
done

echo "== done ($(date -u +%T))"
