#!/usr/bin/env bash
# M4: checks in AWS of what was fixed after the lab sessions of 2026-11-27, against the
# release now deployed (docs/m4/evidence/aws-deploy.txt, "What the lab found"):
#   G  make grafana-check: data sources (Alertmanager through the proxy) and the dashboard
#   R  ADR-0030 rules (10% / 1%, >= 1 request/s) and ADR-0031 buckets are what Prometheus has
#   D  a follower node stopped under workload A: the new smoke-replica passes under load;
#      alert times, 5xx, p99, reads over 1 s
#   P  one clean run of workload A: write and read p99 from Prometheus against the client's
#      (ADR-0031: they should now be close)
#   M  the monitor stopped and started: Prometheus and Loki keep their data
#   T  Telegram notifications sent and failed
#
# Before: the author runs `make release` and `CONFIRM=yes make up-app MONITORING=true
# BENCH=true` (Claude Code's auto-mode checker refuses those for it). After: `make down-app`.
# Run from the repo root:  bash scripts/m4-lab-check.sh
# It writes stop/start calls for one shard node and the monitor, and SSM commands; nothing
# else. Output goes to the terminal and to bench-results/<version>/<time>-m4-lab-check.txt,
# with the account ID masked. Key and password values are never printed. ~45 minutes.
set -uo pipefail
export PATH=$HOME/.local/bin:$PATH
cd "$(dirname "$0")/.."
A() { aws --profile academy --region us-east-1 "$@"; }

ACC=$(A sts get-caller-identity --query Account --output text) || { echo "no AWS credentials (profile academy)"; exit 1; }
ST=$(A cloudformation describe-stacks --stack-name velmakina-kv-app --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE)
case $ST in CREATE_COMPLETE|UPDATE_COMPLETE) ;; *) echo "app stack is $ST: run make release, then CONFIRM=yes make up-app MONITORING=true BENCH=true"; exit 1;; esac
out() { A cloudformation describe-stacks --stack-name velmakina-kv-app --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text; }
V=$(out AppVersion); MON=$(out MonitorInstanceId); CLIENT=$(out BenchmarkClientInstanceId)
case $MON in i-*) ;; *) echo "no monitor in the stack: deploy with MONITORING=true"; exit 1;; esac
case $CLIENT in i-*) ;; *) echo "no benchmark client in the stack: deploy with BENCH=true"; exit 1;; esac

mkdir -p "bench-results/$V"
LOG=bench-results/$V/$(date -u +%Y%m%dT%H%M%SZ)-m4-lab-check.txt
exec > >(sed -u "s/$ACC/<ACCOUNT_ID>/g" | tee "$LOG") 2>&1
TMP=$(mktemp -d); trap 'rm -rf "$TMP"; lsof -ti tcp:9090 -sTCP:LISTEN | xargs kill 2>/dev/null; lsof -ti tcp:3000 -sTCP:LISTEN | xargs kill 2>/dev/null' EXIT

ts() { date -u +%T; }
now() { python3 -c 'import time; print(time.time())'; }
since() { python3 -c "print(f'{$(now) - $1:.0f}')"; }
window() { python3 -c "print(f'{int($(now) - $1) + 10}s')"; }
section() { printf '\n===== [%s] %s (%s UTC) =====\n' "$1" "$2" "$(ts)"; }
RESULTS=()
ms() { python3 -c 'import sys; v = sys.argv[1]; print(v if v == "none" else "%.1f" % (float(v) * 1000))' "$1"; }
hms() { python3 -c 'import sys, datetime as d; v = sys.argv[1]; print(v if v == "none" else d.datetime.fromtimestamp(float(v), d.timezone.utc).strftime("%H:%M:%S"))' "$1"; }
verdict() { RESULTS+=("$1 $2"); echo "   => $1 $2"; }

# SSM port forwarding to the monitor; a working tunnel is kept, a dead one replaced.
tunnel() { # name port
  curl -s -m 3 -o /dev/null "localhost:$2" && return 0
  lsof -ti "tcp:$2" -sTCP:LISTEN | xargs kill 2>/dev/null; sleep 1
  (make "$1" > "$TMP/tunnel-$1.log" 2>&1 &)
  for _ in $(seq 60); do grep -q "Port $2 opened" "$TMP/tunnel-$1.log" 2>/dev/null && return 0; sleep 1; done
  echo "   tunnel $1 did not open"; return 1; }
q() { # PromQL at now (or at $2) -> JSON list of results; reopens the tunnel once
  local r args=(--data-urlencode "query=$1"); [ -n "${2:-}" ] && args+=(--data-urlencode "time=$2")
  r=$(curl -s -m 10 localhost:9090/api/v1/query "${args[@]}") || true
  if ! echo "$r" | grep -q '"status":"success"'; then tunnel prometheus 9090 >/dev/null; r=$(curl -s -m 10 localhost:9090/api/v1/query "${args[@]}"); fi
  echo "$r" | python3 -c 'import json,sys
try: print(json.dumps(json.load(sys.stdin)["data"]["result"]))
except Exception: print("[]")'; }
val() { q "$1" "${2:-}" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["value"][1] if r else "none")'; }
show() { q "$1" "${2:-}" | python3 -c 'import json,sys
for r in json.load(sys.stdin): print("    ", {k:v for k,v in r["metric"].items() if k not in ("__name__","job","instance")}, r["value"][1])'; }
firing() { val "count(ALERTS{alertname=\"$1\",alertstate=\"firing\"$2})"; }
wait_firing() { for _ in $(seq "$4"); do [ "$(firing "$2" "$3")" != none ] && { since "$1"; return; }; sleep 1; done; echo "not within $4"; }
wait_resolved() { for _ in $(seq "$4"); do [ "$(firing "$2" "$3")" = none ] && { since "$1"; return; }; sleep 1; done; echo "not within $4"; }
loki_lines() { # LogQL, start, end (unix s) -> number of lines, through Grafana's proxy
  python3 - "$1" "$2" "$3" <<'EOF'
import base64, json, sys, urllib.parse, urllib.request
qs, start, end = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
h = {"Authorization": "Basic " + base64.b64encode(("admin:" + open("secrets/grafana_password").read().strip()).encode()).decode()}
url = ("http://localhost:3000/api/datasources/proxy/uid/loki/loki/api/v1/query_range?limit=5000&query="
       + urllib.parse.quote(qs) + f"&start={start}000000000&end={end}000000000")
try:
    res = json.load(urllib.request.urlopen(urllib.request.Request(url, headers=h), timeout=20))["data"]["result"]
    print(sum(len(s["values"]) for s in res))
except Exception as e:
    print(0)
EOF
}
p99_client() { # bench log, op (READ/UPDATE) -> client p99 in ms of the first run
  python3 - "$1" "$2" <<'EOF'
import re, sys
for line in open(sys.argv[1]):
    if line.startswith(sys.argv[2] + " ") and "99th(us)" in line:
        us = int(re.search(r"99th\(us\): (\d+)", line).group(1))
        print("%.1f" % (us / 1000)); break
else: print("none")
EOF
}
run_window() { # bench log -> "start_unix end_unix" of the run phase (client times are UTC)
  python3 - "$1" <<'EOF'
import re, sys, datetime
t = {}
for line in open(sys.argv[1]):
    m = re.match(r"== (workload \w+, run 1|done) \((\d\d:\d\d:\d\d)\)", line)
    if m: t[m.group(1).split(",")[0]] = m.group(2)
day = datetime.datetime.now(datetime.timezone.utc).date()
def unix(hms): return int(datetime.datetime.combine(day, datetime.time.fromisoformat(hms), datetime.timezone.utc).timestamp())
print(unix(t["workload a"]), unix(t["done"])) if "workload a" in t and "done" in t else print("none none")
EOF
}

echo "M4 lab check, release $V, $(date -u '+%Y-%m-%d %H:%M UTC'): monitor $MON, client $CLIENT"

section S "smoke test (waits for the nodes)"
for try in 1 2 3; do
  make smoke-aws VERSION="$V" > "$TMP/smoke.log" 2>&1 && break
  echo "   smoke try $try failed; the nodes may still be booting: again in 60 s"; sleep 60
done
grep -E 'version|passed|FAIL|SSM status' "$TMP/smoke.log" | sed 's/^/   /'
grep -q 'smoke test passed' "$TMP/smoke.log" && verdict PASS "smoke-aws" || verdict FAIL "smoke-aws"

tunnel prometheus 9090; tunnel grafana 3000
for _ in $(seq 60); do [ "$(val 'count(up==1)')" = "$(val 'count(up)')" ] && [ "$(val 'count(up)')" != none ] && break; sleep 5; done
echo "   Prometheus targets up: $(val 'count(up==1)') of $(val 'count(up)')"

section G "make grafana-check"
for _ in $(seq 12); do [ "$(loki_lines '{container=~".+"}' $(( $(date +%s) - 600 )) $(date +%s))" -gt 0 ] && break; sleep 10; done
make grafana-check 2>&1 | sed 's/^/   /'; [ "${PIPESTATUS[0]}" = 0 ] && verdict PASS "grafana-check" || verdict FAIL "grafana-check"

section R "rules (ADR-0030) and buckets (ADR-0031) as Prometheus has them"
curl -s localhost:9090/api/v1/rules | python3 -c '
import json,sys
for g in json.load(sys.stdin)["data"]["groups"]:
    for r in g["rules"]:
        if r["name"] in ("ErrorBudgetFastBurn","ErrorBudgetSpent"): print("    ", r["name"], "|", " ".join(r["query"].split()))' | tee "$TMP/rules.txt"
grep -q '0.1 and' "$TMP/rules.txt" && grep -q '> 0.01' "$TMP/rules.txt" && [ "$(grep -c '>= 1' "$TMP/rules.txt")" = 2 ] \
  && verdict PASS "ADR-0030 thresholds and traffic guard loaded" || verdict FAIL "ADR-0030 rules"
LE=$(q 'count by (le) (kv_request_duration_seconds_bucket{job="coordinator"})' | python3 -c 'import json,sys; print(" ".join(sorted({r["metric"]["le"] for r in json.load(sys.stdin)}, key=lambda x: float(x))))')
echo "     coordinator buckets: $LE"
for e in 0.006 0.01 0.02 0.04 0.05 0.015 0.06; do echo " $LE " | grep -q " $e " || MISSING="${MISSING:-} $e"; done
[ -z "${MISSING:-}" ] && verdict PASS "ADR-0031 buckets exported" || verdict FAIL "ADR-0031 buckets missing:${MISSING}"

section D "a follower node stopped under workload A: make smoke-replica"
(make bench-aws WORKLOADS=a RUNS=1 LABEL=check-follower VERSION="$V" > "$TMP/bench-d.log" 2>&1) &
BN=$!
for _ in $(seq 400); do r=$(val 'sum(rate(kv_requests_total{principal="ops",op="read"}[30s]))'); [ "$r" != none ] && python3 -c "import sys; sys.exit(0 if float('$r') > 50 else 1)" && break; sleep 3; done
echo "   run phase: $(val 'round(sum(rate(kv_requests_total{principal="ops"}[30s])))') req/s"
sleep 60
E5=$(val 'sum(kv_requests_total{code=~"5.."})'); E5=${E5/none/0}; TW=$(now)
(make smoke-replica VERSION="$V" > "$TMP/smoke-replica.log" 2>&1; echo "exit=$?" >> "$TMP/smoke-replica.log") &
SR=$!
for _ in $(seq 120); do grep -q '^stopping node' "$TMP/smoke-replica.log" 2>/dev/null && break; sleep 1; done
T0=$(now); NODE=$(grep '^stopping node' "$TMP/smoke-replica.log" | sed -E 's/.*\(([0-9.]+)\).*/\1/')
echo "   stop-instances at $(ts), node $NODE"
echo "   ReplicaDown firing after: $(wait_firing "$T0" ReplicaDown ",node=\"$NODE\"" 240) s"
echo "   NodeDown firing after:    $(wait_firing "$T0" NodeDown ",node=\"$NODE\"" 240) s"
wait $SR; echo "   smoke-replica:"; grep -vE '^Completed|^download' "$TMP/smoke-replica.log" | sed 's/^/     /'
grep -q 'smoke-replica passed' "$TMP/smoke-replica.log" && verdict PASS "smoke-replica under load" || verdict FAIL "smoke-replica under load"
W=$(window "$TW"); E5b=$(val 'sum(kv_requests_total{code=~"5.."})'); E5b=${E5b/none/0}
echo "   5xx over the drill ($W): $(python3 -c "print(int(float('$E5b') - float('$E5')))")"
echo "   reads over 1 s: $(val "sum(increase(kv_request_duration_seconds_count{op=\"read\"}[$W])) - sum(increase(kv_request_duration_seconds_bucket{op=\"read\",le=\"1.0\"}[$W]))")"
echo "   read p99 by shard (s):";  show "histogram_quantile(0.99, sum by (le, shard) (rate(kv_request_duration_seconds_bucket{op=\"read\"}[$W])))"
echo "   write p99 by shard (s):"; show "histogram_quantile(0.99, sum by (le, shard) (rate(kv_request_duration_seconds_bucket{op=\"write\"}[$W])))"
wait $BN; grep -E 'saved to|SSM status' "$TMP/bench-d.log" | sed 's/^/   /'

section P "one clean run of workload A: p99 from Prometheus against the client (ADR-0031)"
sleep 60
make bench-aws WORKLOADS=a RUNS=1 LABEL=check-p99 VERSION="$V" > "$TMP/bench-p.log" 2>&1; echo "   bench exit=$?"
grep -E '^== |READ   -|UPDATE -|saved to' "$TMP/bench-p.log" | cut -c1-160 | sed 's/^/   /'
read -r RS RE <<< "$(run_window "$TMP/bench-p.log")"
if [ "$RS" != none ]; then
  R="$(( RE - RS ))s"
  for op in read:READ write:UPDATE; do
    o=${op%%:*} c=${op#*:}
    pm=$(val "histogram_quantile(0.99, sum by (le) (rate(kv_request_duration_seconds_bucket{op=\"$o\"}[$R])))" "$RE")
    cl=$(p99_client "$TMP/bench-p.log" "$c")
    echo "   $o p99: Prometheus $(ms "$pm") ms, client $cl ms"
  done
  echo "   write p99 by shard (s):"; show "histogram_quantile(0.99, sum by (le, shard) (rate(kv_request_duration_seconds_bucket{op=\"write\"}[$R])))" "$RE"
  echo "   write buckets over the run:"; show "sum by (le) (increase(kv_request_duration_seconds_bucket{op=\"write\",le=~\"0.03|0.04|0.05|0.06|\\\\+Inf\"}[$R]))" "$RE"
  verdict INFO "p99 compared (judge: within the bucket around the client's value)"
else
  verdict FAIL "benchmark run times not found in the output"
fi

section M "the monitor stopped and started: do Prometheus and Loki keep their data?"
L0=$(val 'prometheus_tsdb_lowest_timestamp_seconds'); TB=$(date +%s)
N0=$(loki_lines '{container=~".+"}' $(( TB - 1800 )) $(( TB - 60 )))
echo "   before: Prometheus oldest sample $(hms "$L0") UTC; Loki lines in the 30 min before: $N0"
A ec2 stop-instances --instance-ids "$MON" >/dev/null; A ec2 wait instance-stopped --instance-ids "$MON"; echo "   monitor stopped at $(ts)"
A ec2 start-instances --instance-ids "$MON" >/dev/null; A ec2 wait instance-running --instance-ids "$MON"; echo "   monitor running at $(ts); waiting for its boot script"
lsof -ti tcp:9090 -sTCP:LISTEN | xargs kill 2>/dev/null; lsof -ti tcp:3000 -sTCP:LISTEN | xargs kill 2>/dev/null
for _ in $(seq 40); do sleep 15; tunnel prometheus 9090 >/dev/null 2>&1 && curl -s -m 3 localhost:9090/-/ready | grep -qi ready && break; done
tunnel grafana 3000 >/dev/null 2>&1; for _ in $(seq 20); do curl -s -m 3 localhost:3000/api/health | grep -q '"database"' && break; sleep 6; done; sleep 20
L1=$(val 'prometheus_tsdb_lowest_timestamp_seconds')
N1=$(loki_lines '{container=~".+"}' $(( TB - 1800 )) $(( TB - 60 )))
echo "   after:  Prometheus oldest sample $(hms "$L1") UTC; Loki lines in the same 30 min: $N1"
python3 -c "import sys; sys.exit(0 if '$L0' != 'none' and '$L1' != 'none' and float('$L1') <= float('$L0') + 60 else 1)" && verdict PASS "Prometheus kept its data" || verdict FAIL "Prometheus data (oldest sample moved from $L0 to $L1)"
[ "${N0:-0}" -gt 0 ] && [ "${N1:-0}" -ge $(( N0 * 9 / 10 )) ] && verdict PASS "Loki kept its logs ($N1 of $N0)" || verdict FAIL "Loki logs ($N1 of $N0)"
make grafana-check > "$TMP/gc2.log" 2>&1 && verdict PASS "grafana-check after the restart" || { sed 's/^/   /' "$TMP/gc2.log"; verdict FAIL "grafana-check after the restart"; }

section T "Telegram, as counted by Alertmanager"
id=$(A ssm send-command --instance-ids "$MON" --document-name AWS-RunShellScript --comment "read alertmanager counters" \
  --parameters 'commands=["curl -s localhost:9093/metrics | grep -E \"^alertmanager_notifications(_failed)?_total.*telegram\" | grep -v \" 0$\""]' --query Command.CommandId --output text)
sleep 6; A ssm get-command-invocation --command-id "$id" --instance-id "$MON" --query StandardOutputContent --output text | sed 's/^/   /'
echo "   (counters restart with the monitor's containers: these are since the restart above)"

section summary "results"
printf '   %s\n' "${RESULTS[@]}"
echo "   full output: $LOG"
echo "   next: make down-app"
