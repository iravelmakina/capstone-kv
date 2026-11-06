# M2 monthly budget (2 shards)

> **Draft** — drafted with Claude Code; author to review. Lab budget ($60) read by the author from the lab page.

## Assumptions

- M2 = 1 coordinator + 2 shards, each a **t3.small** in us-east-1 (design decision 6).
- 8 GiB gp3 root volume per instance.
- Coordinator in a public subnet with one public IPv4; shards private, no public IP.
- S3 gateway endpoint is free. M2 is expected to add a NAT Gateway switched on only for
  install windows, about 10 hours a month (ADR-0005).
- CI/CD on **GitHub Actions** (free), because CodePipeline/CodeBuild are blocked in the lab
  (evidence 62). Builds go to an S3 bucket.
- A month = 730 hours. Two scenarios:
  - **A — always on:** 730 h.
  - **B — realistic lab use:** 15 sessions × 4 h = **60 h**. Instances stop when a lab
    session ends, but EBS volumes are billed all month.

## Unit prices

From AWS's public price files, published 2026-11-04 (the AWS Pricing API is blocked in
the lab by an SCP, evidence 45), except where marked.

| Item | Price (us-east-1) |
| --- | --- |
| t3.small, Linux, on-demand | $0.0208 / hour |
| gp3 storage | $0.08 / GB-month |
| Public IPv4 address | $0.005 / hour |
| S3 gateway endpoint | free |
| Interface endpoint (for comparison) | $0.01 / hour per AZ |
| Data between AZs | $0.01 / GB each way |
| NAT Gateway *(VPC pricing page; not in the fetched file)* | $0.045 / hour + $0.045 / GB |
| Application Load Balancer *(EC2 pricing page; not in the fetched file)* | ≈ $0.0225 / hour + usage |

## Our M2 cost

| Item | Formula | A — 730 h | B — 60 h |
| --- | --- | --- | --- |
| 3 × t3.small | 3 × 0.0208 × h | $45.55 | $3.74 |
| 3 × 8 GiB gp3 | 24 × 0.08 (whole month) | $1.92 | $1.92 |
| 1 public IPv4 | 0.005 × h | $3.65 | $0.30 |
| YCSB client (t3.small, ~10 h) | 0.0208 × 10 | $0.21 | $0.21 |
| Cross-AZ traffic (~5 GB of benchmarks) | 5 × 0.02 | $0.10 | $0.10 |
| S3 build bucket (< 1 GB) | | < $0.05 | < $0.05 |
| Our NAT, on ~10 h for installs | 10 × (0.045 + 0.005) | $0.50 | $0.50 |
| t3 CPU credit surplus, worst case | 3 nodes × 2 vCPU × 10 h × $0.05 | $3.00 | $3.00 |
| GitHub Actions | free | $0 | $0 |
| **Ours** | | **≈ $54.98** | **≈ $9.82** |

Option: putting the coordinator behind a load balancer instead of a public IP adds
≈ $16.40 a month always on (≈ $1.35 at 60 h). It would also give M5 a p99 latency metric
for free (see the dissection).

## Already in the lab, not ours

The lab's own stack (for the AWS capstone task) runs a **NAT Gateway with an Elastic IP**,
created at lab start (evidence 67). It bills while it exists, whether we use it
or not:

| Item | Formula | Per month if it stays up | Per day |
| --- | --- | --- | --- |
| NAT Gateway | 0.045 × 730 | $32.85 | $1.08 |
| Its public IPv4 | 0.005 × 730 | $3.65 | $0.12 |
| **Lab baseline** | | **≈ $36.50** | **≈ $1.20** |

It is **not** removed when a session ends: created 2026-11-05 10:44 UTC, still `available`
12 hours later, after a session had ended (evidence 94). So it bills around the clock.
We don't delete it: it belongs to the AWS capstone task.

## Against the lab budget

Lab budget: **$60**, read from the lab page ("Used $0.2 of $60", 2026-11-05). The page
shows one total with no reset date, so we treat it as the budget for the **whole course**,
not per month.

| Scenario | Ours / month | + lab baseline / month | Total / month | $60 lasts |
| --- | --- | --- | --- | --- |
| A — always on, lab NAT always on | $54.98 | $36.50 | **≈ $91** | ≈ 20 days |
| B — realistic, lab NAT always on | $9.82 | $36.50 | **≈ $46** | ≈ 5 weeks |
| B — realistic, if the lab NAT billed only during sessions (60 h) | $9.82 | $3.00 | **≈ $13** | ≈ 4.5 months |

- **Scenario A is ruled out**: it spends the whole course budget in under three weeks.
  Stop instances between sessions (the lab does this anyway) and `make down` anything not needed.
- **What decides it is whether the lab's NAT counts against our $60 outside sessions.**
  It keeps running between sessions (evidence 94), so the likely case is the middle row:
  even realistic use spends the budget in about five weeks. The last row holds only if
  the lab doesn't charge us for its own NAT. Cost Explorer will settle it.
- For comparison: an always-on NAT of our own would add ≈ $37 a month, and three interface
  endpoints across 3 AZs ≈ $66. That's why ADR-0005 keeps the NAT off by default.

## Actual spend vs estimate (task 8.3)

| Date | Source | Actual | Estimate | Notes |
| --- | --- | --- | --- | --- |
| 2026-11-05 22:50 UTC | Lab page | **$0.20** used | ≈ $0.60 (lab NAT up 12 h × $0.05); M1 stack $0 | Meter lags: shows less than the NAT alone has cost (evidence 94) |
| 2026-11-06 (read) | Cost Explorer, daily, credits excluded | Nov 1–4: **$0.00**; Nov 5: **$0.16**; Nov 6: not in yet | Nov 5: ≈ $0.66 (NAT from 10:44 UTC, 13.3 h × $0.05) | Nov 5 is marked incomplete (`**`); only a full day can be compared |

The lab-page figure is behind real usage, so it can't be compared to the estimate yet.
Next reading: Cost Explorer by service once a full day is in (`ce get-cost-and-usage`,
from 2026-11-07). Expected: about $1.20 a day of NAT Gateway and public IPv4, and $0 for
our stack.

## The FinOps loop

Estimate → tag → measure → compare → adjust:

1. **Estimate:** this document.
2. **Tag:** every resource carries `Project=capstone-kv`. The lab can't activate cost
   allocation tags (evidence 68), so tags help find resources, not split the bill.
3. **Measure:** Cost Explorer is readable in the lab (`ce:GetCostAndUsage`, 68), with
   about a day's delay, plus the spend shown on the lab page. AWS Budgets and anomaly
   alerts can't be created (68).
4. **Compare:** after the M1 deploys and again during M2, record actual vs estimate here
   (task 8.3).
5. **Adjust:** if actual spend runs ahead, cut hours first, then instance count.

## Risks

- **t3 CPU credits:** we stay in `unlimited` mode so benchmarks aren't throttled; the
  surplus is budgeted above as a worst case. `CPUCreditBalance` is recorded with every run.
- **M1 itself costs $0:** VPC, subnets, route tables, Internet Gateway and the S3 endpoint are free.
- **Forgotten resources:** EBS, public IPs and a NAT left on keep billing; `make down`
  removes them.
