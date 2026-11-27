# ADR-0032: Scaling metric and thresholds

- Status: **Accepted** — 2026-11-28 by Iryna Velmakina (author) in chat ("Accept and
  commit"), with the AI-review verdicts she confirmed ("Yes, apply them"). Drafted with
  Claude Code; revised after the AI design review (`docs/m5/ai-review.md`).
- Date: 2026-11-27
- Supersedes in part ADR-0031: one more histogram edge (12 ms). Its other edges stay.

## Context

The brief: scale up when p99 latency exceeds a threshold, not on CPU; scale down when
metrics stay below the threshold for N minutes. M4 built the signal: the coordinator's
`kv_request_duration_seconds{op,shard}` histogram, and every replica's
`kv_replica_request_duration_seconds{op}`, scraped every 5 s by the monitor's Prometheus
(ADR-0025, ADR-0029), with recording rules for p99 per shard. What we measured
(`docs/m4/evidence/aws-deploy.txt`, sessions 2 and 3, workload A, 16 client threads,
≈ 900–1,500 requests/s, 2 replica sets):

| | p99 at the coordinator | p99 at the client | SLO |
| --- | ---: | ---: | ---: |
| Read, clean runs | 7.7, 7.9, 9.3 ms | 9.6, 13.7 ms | 15 ms |
| Read, a follower node stopped | 10.6, 11.4 ms | — | |
| Write, after ADR-0031 | 41.0 ms | 42.9 ms | 60 ms |

Write buckets over the clean run: 99.56% at or below 50 ms, 99.82% at or below 60 ms.

Things that shape the choice:

- **p99 from a histogram is exact only at a bucket edge** (ADR-0031). The edges near the
  operating range are 6, 8, 10, 15, 20 ms (reads) and 30, 40, 50, 60 ms (writes). "p99 >
  50 ms" is the same statement as "more than 1% of writes took longer than 50 ms", which
  the counters answer exactly.
- **Under a closed-loop client below saturation, p99 doesn't fall when capacity is added**:
  it is two fsyncs and cross-AZ round trips (M3 AI review 1.4). p99 rises with load only
  once something queues. The thresholds must sit where queueing starts, which we haven't
  measured yet (the M4 runs never saturated the store).
- **What queues first matters.** M2 found the coordinator to be the next bottleneck after
  the leaders' `fsync` lock (`docs/m2/benchmark.md`). If p99 rises because the coordinator
  queues, a new replica set doesn't help (AI review W3). The coordinator measures the whole
  request; the leader measures its own part (including commit wait).
- A node failure raises read p99 too (D''), and scaling can't fix that.
- Migration and leader transfers add load or short write holds (ADR-0033, ADR-0036), which
  shouldn't feed back into more scaling.

## Options — metric

| | 1. CPU (the default everywhere) | 2. p99 per shard at the coordinator, confirmed by the shard's own latency | 3. p99 at the client | 4. Request rate per replica set (a target throughput) |
| --- | --- | --- | --- | --- |
| Brief | **no** ("not on CPU") | yes | yes, but not available to the system | no: not latency |
| Sees what clients feel | indirectly | yes, minus the client's network hop (≈ 2–4 ms on reads) | yes | no |
| Tells "the shard is slow" from "the coordinator is slow" | — | **yes, through the confirmation** | no | — |
| Available to the autoscaler | EC2 metrics | Prometheus, every 5 s | only in the benchmark's output | Prometheus |
| Fooled by | t3 credit throttling, noisy neighbours | a failing node (needs a guard) | — | hot keys |

## Options — thresholds

| | A. At the SLO (15 / 60 ms) | **B. 80% of the SLO, on an edge (12 / 50 ms)** | C. Just above normal (10 / 50 ms) |
| --- | --- | --- | --- |
| Reads: margin over the worst clean run (9.3 ms) | 61% | **29%** | 8% |
| Writes: margin over the clean run (41 ms) | 46% | **22%** | 22% |
| Acts before the SLO breaks | no: scaling takes ≈ 9–12 min while the SLO is already breached | **yes** | yes |
| Exact (on a bucket edge) | yes | **writes yes; reads need a 12 ms edge** | yes |
| False scale-outs from deployment-to-deployment spread (M3: 6.6–10.3 ms) | none | few | likely |

## Decision

**Metric: option 2, with request rate as a guard.** Thresholds: **option B**.

- **Scale out for writes** when a shard's coordinator **write p99 > 50 ms** over a
  2-minute window **and** that shard's leader's own write p99
  (`kv_replica_request_duration_seconds{op="write"}`, commit wait included) **> 40 ms**,
  for **3 minutes**. The second condition says the time is spent in the shard; if the
  coordinator is slow and the leader isn't, the executor reports "coordinator-bound" and
  doesn't scale.
- **Scale out for reads** when a shard's coordinator **read p99 > 12 ms** **and** the read
  p99 of at least one of its replicas **> 8 ms**, for **5 minutes** (longer than writes:
  reads sit closer to their normal values and to what a stopped node does). Kept for
  read-heavy load (workload B), which never moves write p99.
- **Traffic floor:** either trigger needs **≥ 100 requests/s** to that shard over the
  window, so a 2-minute p99 rests on at least ≈ 120 requests above the edge, not a dozen.
- All four thresholds are bucket edges; 12 ms is new: `LatencyBuckets` gains 0.012 (19
  edges; ≈ +10 series on the coordinator).
- **Scale in** when **every** shard's read and write p99 at the coordinator over 5 minutes
  have stayed **below the scale-out thresholds for N = 10 minutes**, and the total request
  rate over those 10 minutes is below **`0.75 × (n − 1) / n`** of the rate recorded when
  the last scale-out started (n = replica sets now). With 3 sets that is half the rate at
  scale-out. The latency is the brief's condition; the rate goes beyond the brief on
  purpose: after a scale-out the latency drops *because* of the new set, so "latency is
  fine" alone would remove it and bring the breach back (defended in the control-question
  notes).
- **Guards** (the executor enforces them, ADR-0035): no action while a replica is down, a
  shard has no leader, or a migration or leader transfer runs, **nor within one rate window
  (5 minutes) after any write hold ended** (a 2 s hold alone lifts a shard's p99 over the
  edge for that long); **10 minutes cooldown** after an action finishes; bounds **2–3
  replica sets**; one action at a time. **Exception:** a scale-in is allowed while a
  replica of the *leaving* set is down, as long as that set still has a leader and 2 live
  replicas, so a damaged shard-3 can still be drained and removed.
- **State:** the rate at the last scale-out and the end time of the last action live in the
  coordinator's migration record (ADR-0033), so an executor restart loses nothing.
- **Where the rules live:** Prometheus rules `ScaleOutDue` and `ScaleInLatencyOk` next to
  the M4 rules, unit-tested with `promtool test rules`, and delivered to Telegram like any
  alert. The executor adds the rate check and the guards.
- **Calibration first.** The first M5 lab task steps a target request rate on 2 replica
  sets (64–128 client threads, so the rate and not the thread count drives the load) and
  records coordinator and leader p99 against the rate, with the coordinator's CPU and
  credits. **If the coordinator saturates before the leaders cross their edges, calibration
  stops** (CPU is an inhibit here, never a trigger). If 12 / 50 ms (and 8 / 40 ms on the
  replicas) don't fall between normal load and saturation, a superseding ADR sets new
  values before the final benchmark.

Why these windows: a scale-out (boot ≈ 3 min, migration ≈ 2–4 min) takes ≈ 5–7 minutes, so
3 minutes of evidence for writes is short against the cost of being late; the 10-minute
cooldown and N are about twice the action's length, so each step's effect is seen before
the next.

**Runner-up:** threshold C with the same guards, if calibration shows the read knee
starts below 12 ms.

## Accepted downsides

- The coordinator doesn't see the client's network hop; a client can be at 14 ms while the
  coordinator reports 10. The SLO is defined at the coordinator too, so they agree.
- Reaction is minutes, not seconds: 3–5 minutes of evidence plus ≈ 5–7 minutes of action
  (≈ 9–12 min in all). A short spike is over before capacity arrives; the spike profile
  measures exactly that, and the result is reported as such.
- One hot key or one slow replica can trigger a scale-out that only partly helps: a new set
  takes a third of every set's keys, not the hot one.
- A coordinator-bound store doesn't scale at all; it is reported, and the coordinator is
  sized up front (ADR-0035) to make that unlikely.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Thresholds never reached, or reached at normal load | Medium | Calibration before the final runs; stop for a new ADR if so |
| Flapping between 2 and 3 sets | Medium | Rate guard, 10-minute cooldown, N = 10 min; a rules test feeds a flap pattern |
| Migration load or a write hold triggers another scale-out | Low | No decisions while a migration runs or within 5 minutes after a hold; copy rate budget (ADR-0033) |
| A failing node is read as "short of capacity" | Medium | Guard: no scaling with a replica down; the ops agent says so (ADR-0034) |
| The coordinator, not the shards, limits p99 | Medium | Leader-side confirmation; t3.medium coordinator; calibration stops on coordinator saturation |

## Related

- ADR-0025, ADR-0031 (buckets; superseded in part), ADR-0026/0030 (SLOs), ADR-0033
  (migration), ADR-0035 (unit and execution), ADR-0034 (agent), ADR-0036 (transfer holds).
