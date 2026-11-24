#!/bin/bash
# Benchmark run on the client instance (ADR-0016), started through SSM by `make bench-aws`:
#
#   bench.sh <bucket> <bench-version> <coordinator-url> [runs]
#
# Downloads kvbench and the workload files from s3://<bucket>/bench/<bench-version>/, loads
# the records once, runs workloads A and B <runs> times each (default 3), and uploads every
# output to s3://<bucket>/bench-results/<bench-version>/<UTC time>/ as soon as it exists,
# so a lab session ending mid-run loses at most the run in progress.
set -euo pipefail

BUCKET=$1 VERSION=$2 URL=$3 RUNS=${4:-3}
REGION=us-east-1
WORK=/opt/kvbench
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
DEST=s3://$BUCKET/bench-results/$VERSION/$STAMP

mkdir -p "$WORK/out"
cd "$WORK"
aws s3 cp --recursive "s3://$BUCKET/bench/$VERSION/" . --region "$REGION" --quiet
chmod +x kvbench

# The same properties for every run; per-run differences come only from the workload file.
COMMON=(-P workloads/common -p "kv.url=$URL")

upload() { aws s3 cp "out/$1" "$DEST/$1" --region "$REGION" --quiet; }

echo "== load ($(date -u +%T))"
./kvbench load "${COMMON[@]}" -P workloads/workloada > out/load.txt 2>&1
upload load.txt
grep -E '^(INSERT|TOTAL)' out/load.txt

for w in a b; do
  for i in $(seq "$RUNS"); do
    f=workload$w-run$i.txt
    echo "== workload $w, run $i ($(date -u +%T))"
    ./kvbench run "${COMMON[@]}" -P "workloads/workload$w" > "out/$f" 2>&1
    upload "$f"
    grep -E '^(READ|UPDATE|TOTAL)' "out/$f"
  done
done

echo "== done ($(date -u +%T)); results in $DEST/"
