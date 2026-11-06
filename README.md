# capstone-kv

A small DynamoDB-like key-value store, built over five milestones for the KSE Cloud
Architectures course (2026/27), in the AWS Academy sandbox. Course brief:
[`docs/course/capstone-brief.md`](docs/course/capstone-brief.md).

**Current milestone: M1 — infrastructure setup.** Spec: OpenSpec change
[`m1-infra-setup`](openspec/changes/m1-infra-setup/proposal.md). Decisions:
[`docs/adr/`](docs/adr/) (numbering starts at 0002: ADR-0001 belonged to lab 1, outside this repo).

## What M1 contains

| Item | Where |
| --- | --- |
| Network stack: VPC `10.20.0.0/16`, 3 public + 3 private subnets in 3 AZs, Internet Gateway, S3 gateway endpoint, one export per ID | [`infra/network.yaml`](infra/network.yaml) |
| View-only policies (one per service area), data/secret deny policy and assumable role (not deployable in the sandbox) | [`infra/iam-readonly.yaml`](infra/iam-readonly.yaml), [`docs/m1/iam-readonly.md`](docs/m1/iam-readonly.md) |
| What the lab role allows and denies, and how we found out | [`docs/m1/labrole-dissection.md`](docs/m1/labrole-dissection.md) |
| Quotas and budget for M2 | [`docs/m1/quotas.md`](docs/m1/quotas.md), [`docs/m1/budget.md`](docs/m1/budget.md) |
| Teardown-and-rebuild log | [`docs/m1/recreate-log.md`](docs/m1/recreate-log.md) |
| AI design review | [`docs/m1/ai-review.md`](docs/m1/ai-review.md) |
| Raw evidence | [`docs/m1/evidence/`](docs/m1/evidence/) |

## Prerequisites

- AWS CLI v2, `cfn-lint`, `make`.
- An AWS Academy lab session. Start the lab, open **AWS Details → AWS CLI: Show**, and put
  the block under `[academy]` in `~/.aws/credentials`; set `region = us-east-1` under
  `[profile academy]` in `~/.aws/config`. Credentials expire with each session.

## Bring it up

```bash
make whoami     # should show assumed-role/voclabs/...
make lint       # static checks, no AWS calls
make diff       # creates a change set and prints it; changes nothing
make up         # same, then asks "Execute? [yes/no]" and waits for the stack
```

The stack is `velmakina-kv-network` in us-east-1. Running cost: **$0** (VPC, subnets,
route tables, Internet Gateway and the S3 gateway endpoint are free).

## Verify it

```bash
make verify     # fails if any private route table has a route to the internet
make drift      # compares the stack's recorded state with what really exists
aws --profile academy --region us-east-1 cloudformation list-exports \
  --query "Exports[?starts_with(Name,'velmakina-kv-network-')].[Name,Value]" --output table
```

The scenarios each check must satisfy are in the
[network spec](openspec/changes/m1-infra-setup/specs/network-foundation/spec.md).

## Tear it down / rebuild

```bash
make down       # delete the stack (delete app stacks first from M2 on)
make rebuild    # timed teardown + rebuild, for the recreate log
```

## What changed since the previous milestone

M1 is the first milestone: everything here is new.

## How this repo is worked on

Spec before code: every milestone starts with an OpenSpec change and ADRs, committed
before any template or code. AI assistance (Claude Code) is logged in
[`AGENT_WORKLOG.md`](AGENT_WORKLOG.md).
