# M2 actual spend vs the M1 budget (task 9.4)

The M1 budget ([`docs/m1/budget.md`](../m1/budget.md)) planned M2 at **≈ $9.82 a month**
(3 × t3.small for 60 lab hours) against a **$60 course budget**, plus the lab's own NAT
Gateway (≈ $1.20 a day, not ours).

## What ran (exact, from CloudFormation history)

Every app stack, from creation to deletion (`list-stacks --stack-status-filter DELETE_COMPLETE`):

| # | When (UTC, 2026-11-24) | Minutes | Instances | Why |
| --- | --- | ---: | ---: | --- |
| 1 | 00:33–00:37 | 4.0 | 3 | first deploy by hand (`docs/m2/evidence/aws-deploy.txt`) |
| 2 | 01:02–01:25 | 22.2 | 3 | CI: first deploy, update, template change, rollback |
| 3 | 10:59–11:07 | 7.3 | 3 (+ client ~3 min) | CI deploy, then the failed benchmark workflow |
| 4 | 11:10–12:40 | 90.0 | 4 | benchmark from the laptop (1 trial pass + the 3-run baseline) |
| 5 | 19:58–20:17 | 19.0 | 3 (+ client ~14 min) | CI deploy after the review fixes, then the green benchmark workflow (1 run) |

**Total: 2.38 hours of app stack, 8.9 instance-hours.**

## Estimated cost (us-east-1 on-demand prices from the M1 budget)

| Item | Usage | Cost |
| --- | --- | ---: |
| t3.small | 8.9 instance-hours × $0.0208 | $0.19 |
| Public IPv4 (coordinator, client) | 4.2 h × $0.005 | $0.02 |
| gp3 root volumes (8 GiB) | 8.9 h × 8 GB × $0.08 / 730 h | $0.01 |
| CPU surplus credits (coordinator) | 0.09 credits | < $0.001 |
| Data between AZs during the benchmarks | ≈ 24 GB × $0.02 (both directions) | ≈ $0.49 |
| S3 (releases ~3.5 MB each, results) | a few MB | < $0.01 |
| **Ours, M2 so far** | | **≈ $0.72** |

**The surprise is data between AZs:** about 6.1 million baseline operations (plus 2.1 million in the check run) of ~2 KB each
crossed from the client (AZ b) to the coordinator (AZ a), and half of them again from the
coordinator to shard-2 (AZ b). The M1 budget assumed 5 GB ($0.10). Two fixes for later runs:
put the benchmark client in the coordinator's AZ (also lowers latency), and remember that M3
replicas in three AZs will make every write cross AZs by design.

## Against the budget

- **Ours:** ≈ $0.72 for all of M2's AWS work, against ≈ $9.82 planned for a month of lab use.
  Instances were up only while needed (`make down-app` after every session).
- **The lab's NAT Gateway** costs more than all of this every day it runs (≈ $1.20/day,
  M1 evidence 94); it isn't ours and we don't delete it.
- **To confirm:** Cost Explorer shows the real figures by service about a day later; the
  lab page shows the total used of the $60.
