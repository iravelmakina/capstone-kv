# M3 actual spend (task 9.3)

> **Draft**, written by Claude Code from CloudFormation history and Cost Explorer, read
> 2026-11-26 after the second session; the author checks it against the lab page.

M3 ran in AWS twice, only for the deploy checks and the benchmark; the app stack was deleted
after each session (`make down-app`). Prices as in the M1 budget and `docs/m2/cost.md`.

## What ran (exact, from CloudFormation history)

| # | When (UTC) | Hours | Instances | What |
| --- | --- | ---: | --- | --- |
| 1 | 2026-11-25 23:05 – 11-26 01:40 | 2.58 | coordinator + 3 nodes, client from 23:12; node-c stopped 00:02–00:33; M2's 2 shards instead of the nodes 00:33–00:57 (incident, evidence §A8) | first deploy, smoke, W = 2 benchmark (A, B), W = 1 benchmark (A) |
| 2 | 2026-11-26 21:18 – 22:33 | 1.26 | coordinator + 3 nodes + client; node-c stopped 21:56–22:32 | control run, follower-down run, catch-up check |

**Total: 3.84 hours of app stack, about 18 instance-hours.**

## The bill (Cost Explorer, by usage type)

Cost Explorer lags by several hours: read on the evening of 2026-11-26, it shows that day
only up to ~16:00 UTC (16 of 24 NAT Gateway hours). So it covers **session 1 fully** and
**session 2 not yet**.

| Usage type | Session 1 (11-25 + 11-26 until 16:00) | Cost | Whose |
| --- | ---: | ---: | --- |
| `BoxUsage:t3.small` | 11.3 h | $0.24 | ours |
| `DataTransfer-Regional-Bytes` | 75.8 GB | $0.76 | ours: replication and benchmark traffic between AZs |
| `CPUCredits:t3` | 0.5 vCPU-h | $0.03 | ours: surplus credits |
| `EBS:VolumeUsage.gp3` | | $0.01 | ours |
| `PublicIPv4:InUseAddress` | ~4.6 h of 44.7 h | $0.02 | ours: coordinator and client; the rest is the lab's NAT address |
| `NatGateway-Hours` (+ its address) | 40 h | $2.00 | **the lab's** NAT Gateway in "Project VPC" |
| **Session 1, ours** | | **≈ $1.06** | |

**Session 2, estimated** the same way: 5.7 instance-hours ($0.12), about half of session
1's benchmark traffic, ~38 GB ($0.38), surplus credits (~22 credits, $0.02, evidence §A19),
addresses ($0.01): **≈ $0.53**.

**M3, ours: ≈ $1.6**, of which about **70% is data between AZs**. That is the price of the
design: every write goes from the leader to two followers in other AZs and every read
fans out to two replicas, so M3 moves roughly 2–3× M2's bytes across AZs per operation (M2
billed 36 GB for its whole benchmark, M3 session 1 alone 76 GB).

## Against the budget

| | |
| --- | ---: |
| Course budget | $60.00 |
| Used before M3 (lab page, 2026-11-24, `docs/m2/cost.md`) | $23.40 |
| Account total, Cost Explorer, 2026-11-01 to 11-26 ~16:00 UTC | $27.27 |
| Used now (lab page) | **$ __ (to read from the lab page)** |

- **Ours stays small:** ≈ $0.40 for M2 and ≈ $1.6 for M3.
- **The lab's NAT Gateway dominates:** $22.95 of the $27.27 so far, at $1.08 a day plus
  $0.12 for its address, 24 hours a day whether or not the lab is running (as found in M2).
- **Runway:** with ≈ $32.7 left (Cost Explorer) and ≈ $1.20 a day of lab NAT, the credit
  lasts until about **2026-12-23**, minus what M4 and M5 spend. M4's monitoring instance
  (Prometheus and Grafana, one t3.small only during sessions) adds cents; cross-AZ
  benchmark traffic is the item to watch.
