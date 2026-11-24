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

**The surprise is data between AZs:** about 6.1 million baseline operations (plus 2.1
million in the check run) of ~2 KB each crossed from the client (AZ b) to the coordinator
(AZ a), and half of them again from the coordinator to shard-2 (AZ b). The M1 budget assumed
5 GB ($0.10); the bill shows 36 GB (below). Two fixes for later runs: put the benchmark
client in the coordinator's AZ (also lowers latency), and remember that M3 replicas in three
AZs will make every write cross AZs by design.

## The actual bill (Cost Explorer, 2026-11-01 to 2026-11-24, read 2026-11-24 evening)

The lab page says **Used $23.4 of $60**. Cost Explorer (`ce get-cost-and-usage`, by usage
type) adds up to the same $23.4:

| Usage type | Quantity | Cost | Whose |
| --- | ---: | ---: | --- |
| `NatGateway-Hours` | 459 h | $20.66 | **the lab's** NAT Gateway in "Project VPC" (the lab stack's VPC, not ours) |
| `PublicIPv4:InUseAddress` | 461.9 h | $2.31 | ~459 h the lab NAT's address, ~3 h ours |
| `DataTransfer-Regional-Bytes` | 36.1 GB | $0.36 | ours: benchmark traffic between AZs ($0.01/GB) |
| `BoxUsage:t3.small` | 1.2 h | $0.02 | ours |
| `APIRequest` | 1 | $0.01 | ours |
| **Total** | | **≈ $23.4** | ours ≈ $0.40, the lab's ≈ $23.0 |

- **98% of the spend is the lab's NAT Gateway and its address** (≈ $1.20 a day, M1 evidence
  94). It isn't ours and, under the project rules, we don't touch it.
- **Stopping the lab doesn't stop it.** The NAT Gateway was created 2026-11-05 10:44 UTC
  and Cost Explorer bills it **24 hours every day** since (14 h on the 5th), including the
  days the lab was stopped. Ending a lab session stops EC2 instances; a NAT Gateway has no
  "stopped" state, it runs until deleted.
- **Our real cost is ≈ $0.40**, below the estimate above: data between AZs was billed at
  $0.01/GB, not $0.02, and Cost Explorer lists only 1.2 t3.small hours against the 8.9 the
  CloudFormation history shows. I can't explain that gap (it may still be catching up);
  either way the instances cost under $0.20.
- **Budget risk:** $36.6 is left. At $1.20 a day the lab NAT alone uses it up in about 30
  days, around 2026-12-24, before M3–M5 are done. Ours is small next to it. Whether the NAT
  may be stopped is a question for the lecturer, not something we change.

## Against the budget

- **Ours:** ≈ $0.40 billed (≈ $0.72 estimated) for all of M2's AWS work, against ≈ $9.82
  planned for a month of lab use. Instances were up only while needed (`make down-app`
  after every session).
- **The lab's:** ≈ $23.0 so far, nearly all of the $23.4 used.
