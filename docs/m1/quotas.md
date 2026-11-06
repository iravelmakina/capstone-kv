# Quotas that matter for M2

> **Draft** — drafted with Claude Code from discovery evidence; author to review.

AWS Academy lab, us-east-1, read on 2026-11-05. Evidence: `evidence/` (numbers in brackets are evidence sections).
Two kinds of limit apply: **AWS service quotas** (read with Service Quotas) and the
**lab's own policy limits** (found with dry-runs and the simulator). The lab limits are
far stricter, so they're the ones that bite.

## What M2 needs

Coordinator + 2 shards = 3 × t3.small (2 vCPU each), plus a YCSB client while
benchmarking. Looking ahead: M3 (3 replicas per shard) = 2 × 3 + coordinator + client
= **8 instances, 16 vCPU**.

## Limits

| Limit | Value | Used / M2 need | Bites? | Source |
| --- | --- | --- | --- | --- |
| **Instance types** | t3.small, t3.medium allowed; t3.large and up denied | t3.small | **Yes:** no vertical scaling past t3.medium | lab policy (37–40) |
| **ECS / Fargate** | denied | not used | **Yes:** rules out containers on Fargate | simulator (08) |
| **AWS CI/CD** (CodePipeline, CodeBuild, CodeDeploy, OIDC provider) | denied | M2 needs a pipeline | **Yes:** the pipeline must run outside AWS (GitHub Actions) | simulator (62) |
| **ECR** | denied | container images, if used | **Yes:** images come from Docker Hub or as builds via S3 | simulator (62) |
| **Instance permissions** | instances can read S3 but not write it, and can't send CloudWatch metrics or logs | M2 nodes fetch builds from S3 | Not in M2; matters in M3–M4 | simulator (63) |
| **Create IAM roles / pass roles** | denied, except passing `Inventory-App-Role` | instances must use `Inventory-App-Role` | **Yes:** no custom least-privilege instance role | simulator (08, 20), dry-run (41) |
| **Lab session** | 3 h per session; credentials expire at the end; stacks survive (recreate log); instances are expected to stop (not yet observed) | — | **Yes:** for long benchmarks and 24×7 uptime | lab page; Academy docs |
| Running instances | not measured | 4 (M2), 8 (M3) | Unknown — check the lab page | — |
| Standard-instance vCPUs | 256 | 8 (M2), 16 (M3) | No | Service Quotas (48) |
| VPCs per region | 5 | 2 used + 1 ours | No, but a rebuild needs the slot free | (47, 14) |
| Internet gateways per region | 5 | 2 used + 1 ours | No | (47, 54) |
| Gateway VPC endpoints per region | 20 | 1 | No | (47) |
| Interface endpoints per VPC | 50 | 0 (M4 maybe 2–3) | No | (46) |
| Elastic IPs | 5 | 0, or 1 while the NAT is on | No | (19) |
| Subnets / route tables per VPC | 200 / 200 | 6 / 4 | No | (46) |
| Security groups per region | 2,500 | ~3 | No | (47) |
| gp3 storage | 50 TiB | ~24 GiB | No | (49) |
| CloudFormation stacks | 2,000 | 1 (M1), ~2 (M2) | No | (50) |

## Takeaways for M2

- Scale **out** with more t3.small/medium nodes; scaling **up** stops at t3.medium.
- Instances must run with `Inventory-App-Role`. It has `ReadOnlyAccess` and Secrets
  Manager access, which is more than the shards need. Accepted, since the lab allows no
  other role; recorded in the dissection.
- Long benchmarks must fit inside one lab session.
- The M2 pipeline runs on GitHub Actions with lab credentials refreshed each session.
