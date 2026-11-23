# ADR-0014: App infrastructure

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

M2 needs the coordinator, two shards and a benchmark client running in the M1 network,
described as IaC. Constraints: t3.small/medium only, instances must use the
`Inventory-App-Role` instance profile, budget $60 for the course (M1 budget), network IDs
come from the M1 exports (ADR-0003). Clients (benchmark, my laptop) must reach the
coordinator; shards must not be reachable from outside.

## Options

| | 1. Instances in CloudFormation | 2. Auto Scaling groups now | 3. Coordinator behind a load balancer |
| --- | --- | --- | --- |
| Fits M2 (fixed 2 shards) | yes | yes, more parts | yes |
| Cost | 3 × t3.small, only while up | same | + ≈ $16/month |
| Ready for M5 autoscaling | no, rewritten then | yes | partly |
| Stable shard IDs | yes (parameter per instance) | needs a scheme | — |

## Decision

**Option 1, in new stacks next to the network:**

| Stack | Holds | Lifetime |
| --- | --- | --- |
| `velmakina-kv-artifacts` | S3 bucket for image tarballs and benchmark results (`DeletionPolicy: Retain`) | long-lived |
| `velmakina-kv-app` | coordinator (public subnet A, public IP), shard-1 (private A), shard-2 (private B), security groups | up only during a lab session |

- **t3.small, `unlimited` CPU credits** (M1 budget); Amazon Linux 2023 AMI from the public SSM parameter.
- **Parameters:** `AppVersion` (commit SHA of the image), `AllowedClientCidr` (my IP), shard count expected by the coordinator.
- **Security groups:** coordinator accepts 8080 from the VPC CIDR and `AllowedClientCidr`;
  shards accept 8080 **only from the coordinator's security group**. No SSH: admin access
  through SSM on the coordinator.
- **Benchmark client:** a t3.small in public subnet B, in the app stack behind a condition,
  on only for benchmark runs.

**Runner-up:** option 2. Reconsider in M5, where autoscaling needs it.

## Accepted downsides

- M5 will replace the shard instances with an Auto Scaling group.
- The coordinator has a public IP ($0.005/h) instead of a load balancer.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| App stack left running between sessions | Medium | `make down-app` at the end of each session; README checklist |
| Network can't be deleted while the app imports it | Low | Teardown order app → network (ADR-0003); `make down` does it |

## Related

- Implemented in: `infra/artifacts.yaml`, `infra/app.yaml`, `Makefile`.
- Uses: ADR-0003 (exports), ADR-0004 (subnets), ADR-0009 (image delivery).
