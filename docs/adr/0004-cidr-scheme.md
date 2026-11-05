# ADR-0004: CIDR scheme, region and AZs

- Status: **Accepted** — 2026-11-05 by Iryna Velmakina (author). Drafted with Claude Code; revised after an independent AI design review (`docs/m1/ai-review.md`).
- Date: 2026-11-05

## Context

- M3 puts 3 replicas of each shard in different AZs; M5 adds nodes.
- Subnets can't be resized, and exported subnets are costly to replace (ADR-0003).
- Must not overlap: the lab's own "Project VPC" in this account (`10.0.0.0/16`), the
  default VPC (`172.31.0.0/16`), Docker (`172.17.0.0/16`), local Kubernetes
  (`10.244.0.0/16`, `10.96.0.0/12`).

## Options

| | 3 AZs, /20 subnets | 2 AZs, /24 subnets | Smaller VPC |
| --- | --- | --- | --- |
| Survives losing an AZ with 3 replicas | yes — 2 of 3 left | no — one AZ holds 2 replicas | depends |
| Usable IPs per subnet | 4,091 | 251 | fewer |
| Room to add an AZ or tier | yes | yes | little |

## Decision

**VPC `10.20.0.0/16`, 3 AZs, /20 subnets** from `Fn::Cidr [10.20.0.0/16, 8, 12]`:

| CIDR | Use |
| --- | --- |
| `10.20.0.0/20`, `10.20.16.0/20`, `10.20.32.0/20` | public, AZ a / b / c |
| `10.20.48.0/20` | spare: public, 4th AZ |
| `10.20.64.0/20`, `10.20.80.0/20`, `10.20.96.0/20` | private, AZ a / b / c |
| `10.20.112.0/20` | spare: private, 4th AZ |
| `10.20.128.0/17` | reserved: future tiers |

**Region `us-east-1`:** allowed by the lab (us-west-2 too; other regions are denied).
Six AZs, same prices as us-west-2, and lower latency from Kyiv, which matters because
some clients run from my laptop: the M4 MCP client, the M5 ops agent, and my own testing.

**AZs are template parameters, chosen by AZ ID**, because AZ names map to different
physical AZs in each account. `use1-az3` is the only AZ without t3.small
(evidence 12–13):

Chosen AZ IDs: **`use1-az1`, `use1-az2`, `use1-az4`**

**Runner-up:** 3 AZs with /24 subnets. Works just as well today; reconsider only if
address space ever becomes scarce (e.g. peering).

## Accepted downsides

- /20 is far more than needed.
- us-east-1 is the busiest region, with the worst outage history.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| An AZ runs out of t3.small capacity | Medium | Three other AZs exist; but changing an AZ replaces subnets, so choose before the first deploy |
| Region-wide us-east-1 outage | Low | Accepted. Moving to us-west-2 is possible but not trivial: new AZ IDs and a new t3.small check are needed |

## Future optimizations

- Add a 4th AZ from the spare blocks, or a new tier from the reserved /17. Additive with
  per-value exports (ADR-0003).

## Related

- Implemented in: `infra/network.yaml` (VPC CIDR, `Fn::Cidr` subnet blocks, AZ ID parameters).
- Uses: ADR-0003 (exports), ADR-0005 (private route tables).
