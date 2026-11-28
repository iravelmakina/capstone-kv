# M5: a whole autoscaling cycle on the local cluster

> Written by Claude Code on 2026-11-28 from a run it made itself on the author's laptop
> (task 6.4's local check; the AWS run with YCSB is task 8.x). Times are UTC.

**Setup:** `make local-up` with monitoring (coordinator, shard-1 and shard-2 with three
replicas each, Prometheus, Alertmanager), `kvscale --capacity compose` (it starts and stops
the Compose services `r3a`–`r3c` instead of Auto Scaling groups), release built from the
working tree. Load: `scripts/local-load.sh 64 600`, 64 parallel writers for 10 minutes.
Every 15 s the run printed the recording rules' values and kvscale's decision; kvscale's
own JSON log is the source of the decisions below.

## Scale-out

| Time | What happened |
| --- | --- |
| 16:17:45 | load starts; 2 sets |
| 16:19:49 | load at its plateau: ≈ 290 + 275 req/s on shard-1 / shard-2; write p99 at the coordinator 220 / 198 ms, the leaders' own write p99 the same (cause check passes) |
| 16:21:35 | `ScaleOutDue` fires (both shards, write trigger, after its 3 minutes) |
| 16:21:36 | kvscale: `launch` — "scale-out due for [shard-1/write shard-2/write]" (`docker compose --profile shard3 up -d r3a r3b r3c`) |
| 16:21:53 | kvscale: `join` — "shard-3 registered and serves: migrate its keys in" (17 s after launch) |
| 16:22:05–16:22:35 | migration `add-shard-3-…`: copying, then cutover; shard-3 is on the ring at 16:22:35 and takes ≈ 1/3 of the requests within two minutes (shard-3 rate 17 → 195 req/s) |
| 16:22:35 on | 3 sets; `ScaleOutDue` keeps firing but kvscale is at its upper bound (3 sets) and does nothing more |

- **No errors:** 335,078 writes in 600 s (≈ 558/s), every one answered 200, including those
  during the migration's cutovers.
- **Latency did not drop locally**, and this is expected here: write p99 stayed at 190–250
  ms on all three sets and the total rate stayed at ≈ 560 req/s. All nine replicas share one
  laptop's disk and CPU, and each write is an fsync on two of them; a third set adds leaders
  but no disk. Locally this run shows the control loop (trigger, launch, join, migration,
  upper bound), not a capacity gain; the gain has to be shown in AWS, where each replica has
  its own instance and volume (task 8.x). Local p99 is also ≈ 4× AWS's (M3: 39 ms), so the
  50 ms edge is breached at any load here.
- **The recorded join rate is a 10-minute average:** kvscale logged 216–232 req/s at the
  join while the real load was ≈ 565 req/s, because the window still held the minutes
  before the load. The scale-in rule compares against that number, so after a fast ramp it
  is stricter than ADR-0032 intends (scale-in needs the load under ≈ 116 req/s instead of
  ≈ 280): it errs on the side of keeping capacity, never of removing it too early. Noted
  for the AWS session; a shorter window at the join would follow a ramp better.

## Scale-in

| Time | What happened |
| --- | --- |
| 16:27:55 | load ends; 3 sets |
| 16:28–16:42 | kvscale: "3 sets; latency not below the thresholds for 10 minutes yet" every 15 s. The 5-minute p99 window still holds the load until ≈ 16:33; with no requests the recording rule has no series, which `ScaleInLatencyOk` counts as below (`absent(...)`); then its 10 minutes |
| 16:42:59 | kvscale: `leave` — "every shard below the thresholds for 10 minutes at 0 req/s" (the rate guard holds too: 0 < 0.5 × 232). Cooldown since the join (20 min) long over |
| 16:43:14 | migration `remove-shard-3-…` copying shard-3's keys back to shard-1 and shard-2 |
| 16:43:45 | shard-3 retired; kvscale: `stop` (`docker compose --profile shard3 stop r3a r3b r3c`) |
| 16:44:00 | kvscale: `forget` — the coordinator drops shard-3 |
| 16:44:15 | "2 sets; no scale-out condition": back where it started |

- **Scale-in came 15 minutes after the load stopped**, as ADR-0032 sets it: the 5-minute
  p99 window plus 10 minutes below. The leave migration took under 46 s.
- **Every step was one decision from the current state** (launch, join, leave, stop,
  forget), so a kvscale restarted at any point would have carried on from there.
- Not covered locally: a scale-in under light but non-zero load (the rate guard deciding),
  and a guard blocking a step (`go test ./internal/scale/` covers both); a sudden spike and
  sustained load with YCSB are the AWS run.
