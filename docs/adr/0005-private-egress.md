# ADR-0005: Outbound access from private subnets

- Status: **Accepted** — 2026-11-05 by Iryna Velmakina (author). Drafted with Claude Code; revised after an independent AI design review (`docs/m1/ai-review.md`).
- Date: 2026-11-05

## Context

The brief: private subnets "are for internal access only". M1 runs no instances, so
nothing needs outbound access yet. From M2, nodes in private subnets will need to install
software and fetch builds. The lab allows NAT Gateways, Elastic IPs and endpoints
(evidence 33–36, 42); instances can read S3 but can't call CloudWatch (63).

## Options

Prices: us-east-1, 730 h/month (see `budget.md`).

| Option | Per month | Problem |
| --- | --- | --- |
| 1. No route out; S3 gateway endpoint only | $0 | Anything not in S3 can't be installed |
| 2. NAT Gateway, always on | ≈ $37 (+ data) | Likely more than all our compute |
| 3. NAT Gateway, off by default, switched on by a template parameter | $0 idle; ≈ $0.05/h while on | Someone has to switch it on and off |
| 4. NAT instance (t3.nano) | ≈ $4 | Uses an instance; I patch it; single point of failure |

## Decision

**M1: option 1.** Private subnets have no route out; the only exit is a free S3 gateway
endpoint. This is exactly "internal access only" and costs nothing.

**M2 (planned, decided in M2's ADR):** option 3, when there are nodes to install software
on. Adding it is additive: one NAT Gateway, one Elastic IP and a route per private table,
no re-addressing.

**Runner-up:** option 3 already in M1. Rejected because nothing in M1 needs it, and it
adds a condition and three resources to explain.

## Accepted downsides

- The M2 network stack update will add the NAT.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Someone adds a default route by hand | Low | `make verify` fails if any private route table has `0.0.0.0/0` |

## Related

- Implemented in: `infra/network.yaml` (private route tables with no `0.0.0.0/0`; one
  `AWS::EC2::VPCEndpoint` of type `Gateway` for S3).
- AI review W1 (accepted, deferred to M2): `docs/m1/ai-review.md`.
