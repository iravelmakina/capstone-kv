# Design

> **Approved** by the author on 2026-11-05. Drafted with Claude Code; revised after the
> AI design review (`docs/m1/ai-review.md`).

## Context

- AWS Academy lab (role `voclabs`, temporary credentials). Facts in
  `docs/m1/labrole-dissection.md` and `docs/m1/evidence/`.
- "LabRole" in the brief is a generic name (lecturer, 2026-11-05). Here it means
  `voclabs`, the role we act as, and `Inventory-App-Role`, the role attached to
  instances. IAM roles and policies can't be created; ECS is denied.
- Region: **us-east-1** (ADR-0004). AZs `use1-az1/2/4`.
- Later needs: 2 shards + coordinator (M2), 3 replicas per shard across AZs (M3),
  autoscaling (M5).

## Goals / Non-Goals

**Goals:** a network M2–M5 can build on with additive updates only; everything
rebuildable with one command; $0 running cost.

**Non-Goals:** bastion, VPN, peering, IPv6; deploying the IAM template.

## Decisions

1. **IaC: CloudFormation.** ADR-0002.
2. **Layout: separate network stack, one export per value; IAM template never deployed.** ADR-0003.
3. **CIDR: `10.20.0.0/16`, /20 subnets, 3 AZs chosen by AZ ID.** ADR-0004.
4. **Egress: none in M1; a free S3 gateway endpoint is the only exit.** A switchable NAT is planned for M2. ADR-0005.
5. **IAM: our own view policy plus a separate Deny policy for data and secrets; role
   trusted by one MFA-signed-in user or role.** ADR-0006.
6. **M2 compute assumption: EC2 t3.small**, needed now only for the budget and quotas.
   The binding ADR comes in M2. EBS keeps shard data; t3.small is allowed
   (dry-run); cheapest. *Rejected:* Lambda (stateless, cold starts skew p99); Fargate
   (`ecs:CreateCluster` is denied in this lab).
7. **No security groups in M1.** They come with the M2 app stack, where real ports exist:
   role groups (load balancer → coordinator → storage, storage → storage for replication)
   that reference each other rather than CIDRs. *Rejected:* one group open to the whole VPC
   CIDR in M1 (makes the network flat: public subnets could reach data nodes).
8. **Makefile around CloudFormation:** `diff` creates a change set and prints it without
   executing; `up` does the same and executes only after confirmation; `down`; `drift`;
   `verify` checks the private route tables directly; `rebuild` times down + up.

## Conventions

- Every resource is tagged `Project=capstone-kv`; stacks and names start with `velmakina-`.
- AWS CLI calls use profile `academy`.
- Committed evidence has account IDs replaced with `<ACCOUNT_ID>`.

## Risks / Trade-offs

- [The lab's **Reset** wipes everything] → never press it; the repo can rebuild everything anyway.
- [The AWS capstone shares this account, with its own VPC and stack] → our own CIDR and
  `velmakina-` prefix; never touch the lab's resources.
- [Credentials expire each lab session] → manual deploys in M1; CI/CD credentials decided in M2.
- [Exports block deleting the network] → teardown order: app first.
- [Instances limited to t3.medium; ECS denied] → fits M2–M3; M5 scale-out uses more t3 nodes.
- [The simulator may not reflect every policy layer] → every simulator result that matters is confirmed by a dry-run.
- [t3 CPU credits: in `standard` mode a long benchmark drains credits and throttles,
  skewing p99 and M5 scaling] → stay in `unlimited` mode, budget the surplus, and record
  `CPUCreditBalance` (an AWS-published metric) with every benchmark.

## Migration Plan

New repo. Rollback is `make down`.

## Open Questions

- Lab budget found: $60 (`docs/m1/budget.md`). Still open: is it per course or per month,
  and does the lab's own NAT Gateway count against it (ask the lecturer; Cost Explorer).
