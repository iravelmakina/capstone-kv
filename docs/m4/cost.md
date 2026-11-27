# M4 actual spend (task 9.4)

> Written by Claude Code on 2026-11-27, right after the second lab session, from
> CloudFormation and CloudTrail history and Cost Explorer. Cost Explorer only reaches
> 2026-11-27 07:00 UTC at this point, so M4's sessions aren't billed in it yet: the figures
> below are estimates from hours and M3's measured rates, marked as such. Prices as in the
> M1 budget and `docs/m3/cost.md`.

## What ran (exact, from CloudFormation and CloudTrail)

| # | When (UTC) | Hours | Instances | What |
| --- | --- | ---: | --- | --- |
| 0 | 2026-11-27 12:49 – 13:07 | ~0.03 | monitor, client, coordinator for a minute or two | first create rolled back (an apostrophe in a rule description), deleted |
| 1 | 2026-11-27 13:07 – 15:42 | 2.58 | coordinator, monitor, 3 nodes, client: 6 × t3.small | deploy, fixes, smoke, drills D1–D3, Loki trace. The session ended about 15:17; the lab stopped the instances at 15:42 (CloudTrail, `StopInstances` by the lab's account) |
| – | 15:42 – 18:18 | (2.6) | all 6 stopped: disks only | between sessions |
| 2 | 2026-11-27 18:18 – 19:16 | 0.97 | the same 6 (the lab started them, CloudTrail `StartInstances`) | D1 under load, benchmark A × 3, then `make down-app` (stack deleted 19:18) |

Drill stops (one node for 1–5 minutes, three times) are left out: under 0.2 instance-hours.

**Total: 3.6 hours of app stack, about 21 instance-hours** (M3: 3.96 hours, 18.5
instance-hours, with one instance fewer).

## The bill, estimated

| Usage type | Quantity | Cost | How estimated |
| --- | ---: | ---: | --- |
| `BoxUsage:t3.small` | ~21 h | ~$0.44 | hours above × $0.0208 |
| `DataTransfer-Regional-Bytes` | ~30–45 GB | ~$0.30–0.45 | M3 billed ~8 GB per benchmark run of this size between AZs (76 GB for 9 runs in session 1); M4 ran 5 runs of workload A (one in D2, one in D1', three in the benchmark) plus their loads. Metrics scrapes and log shipping add little: kilobytes per scrape every 5 s |
| `PublicIPv4:InUseAddress` | ~11 h | ~$0.05 | coordinator, client and monitor while running, $0.005/h |
| `CPUCredits:t3` | | ~$0.03 | as in M3: surplus credits during benchmarks |
| `EBS:VolumeUsage.gp3` | | ~$0.03 | 6 volumes for ~6.2 h, including the 2.6 h stopped |
| Secrets Manager | 4 secrets | ~$0.05 a day | $0.40 per secret a month, **for as long as they exist**, lab running or not |
| S3 (artifacts bucket) | 193 MB | < $0.01 | |
| **M4, ours (sessions)** | | **≈ $0.9–1.0** | |

Like M3, most of it is instance time and data between AZs. The monitoring itself (one
more t3.small, scrapes, logs) is ≈ $0.07 of it. The new item that keeps counting is
**Secrets Manager: ≈ $1.60 a month** while the four `velmakina-kv/*` secrets exist. They
cost nothing to use, but their monthly fee doesn't stop when the lab does.

## Against the budget

| | |
| --- | ---: |
| Course budget | $60.00 |
| Used before M4 (lab page, 2026-11-26 night, `docs/m3/cost.md`) | $27.30 |
| Account total, Cost Explorer, 2026-11-01 to 11-27 ~07:00 UTC | $28.65 |
| M4 sessions, not billed yet (estimate) | ≈ $1.0 |
| The lab's NAT Gateway for the rest of 11-27, not billed yet | ≈ $0.9 |
| **Used by the end of 11-27 (estimate)** | **≈ $30.5** |
| **Left (estimate)** | **≈ $29.5** |

- **The lab's NAT Gateway still dominates:** $1.08 a day plus $0.12 for its address, as
  since M2. That's ≈ $24 of the ≈ $30.5.
- **M4 cost about as much as M3** (≈ $1.0 against ≈ $1.6), with one more instance: fewer
  benchmark runs, so less data between AZs.
- **Runway:** ≈ $29.5 at ≈ $1.25 a day (lab NAT, plus our secrets) lasts until about
  **2026-12-20**, minus what M5 spends. M5 has a benchmark and autoscaling experiments, so
  cross-AZ traffic and instance hours are the items to watch, as before.
- **Not checked yet:** the lab page's own figure (read by the author) and Cost Explorer
  once it has caught up with 11-27. Both lag by several hours.
