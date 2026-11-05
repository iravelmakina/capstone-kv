# Proposal

> **Approved** by the author on 2026-11-05. Drafted with Claude Code; revised after the
> AI design review (`docs/m1/ai-review.md`).

## Why

From M2 on, the KV store needs a network it can be deployed into again and again, and a
clear picture of what the sandbox allows. M1 builds that base as code before any app exists.

## What Changes

- **Network stack:** VPC with public and private subnets in 3 AZs, an Internet Gateway for
  the public ones, private subnets with no route out (a free S3 gateway endpoint is the
  only exit), and one export per ID.
- **IAM template:** a view-only policy, a separate deny policy for data and secrets, and a
  role another principal can assume. Linted, not deployed: the sandbox refuses
  `iam:CreateRole`, and recording that is part of the task.
- **"LabRole" dissection:** what the Academy lab roles allow and deny, and how we found
  out. The brief's "LabRole" is a generic name (lecturer); here it's `voclabs` (the role
  we act as) and `Inventory-App-Role` (the instance role).
- **Evidence:** M2 quotas, M2 budget for 2 shards, a timed teardown-and-rebuild log, and
  the AI design review.
- **ADRs:** 0002 IaC tool, 0003 stack layout, 0004 CIDR and region, 0005 private egress,
  0006 read-only policy scope and trust.

## Capabilities

### New Capabilities

- `network-foundation`: VPC, subnet tiers, routing, the S3 endpoint and exported IDs.
- `iam-readonly-access`: a view-only policy and an assumable role, as code.
- `infra-lifecycle`: destroy and recreate everything from the repo, review changes before they run, and prove it.

### Modified Capabilities

None (new repository).

## Impact

- New files: `infra/network.yaml`, `infra/iam-readonly.yaml`, `Makefile`, `docs/adr/*`,
  `docs/m1/*`, `README.md`.
- AWS: one stack, `velmakina-kv-network`, in us-east-1 in the AWS Academy lab. Costs $0.
- Tools: AWS CLI v2, `cfn-lint`, OpenSpec CLI.
- M2 app stacks will import the network's exports. Private subnets are internal-only
  (ADR-0005); M2 is expected to add a switchable NAT.
