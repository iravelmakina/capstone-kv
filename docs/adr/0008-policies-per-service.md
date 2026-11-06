# ADR-0008: View policies grouped by service

- Status: **Accepted** — 2026-11-06 by Iryna Velmakina (author). Drafted with Claude Code;
  follows the lecturer's advice in the 02.11 defenses to keep separate policies per service type.
- Date: 2026-11-06
- Supersedes: ADR-0006's "Policy: option 3, **as two policies**". The separate Deny
  policy, the trust rules (ADR-0006) and the named actions (ADR-0007) stand.

## Context

`velmakina-view` is one policy with ~45 named actions across seven services. It works,
but one big policy is hard to review, and the only way to give someone part of it (only
the network, say) is to copy it.

## Options

| | 1. One view policy (now) | 2. One view policy per service group | 3. One policy per AWS service |
| --- | --- | --- | --- |
| Policies on the role | 1 + Deny | 6 + Deny | ~10 + Deny |
| Read and justify one part | hard: one long list | easy: each is short | easy |
| Reuse a part (e.g. only network) | copy and edit | attach that policy | attach that policy |
| Role limit: 10 managed policies by default | fine | fine (7) | at the limit |

## Decision

**Option 2.** Six view policies, each for one area, plus the unchanged Deny policy:

| Policy | Covers |
| --- | --- |
| `velmakina-view-network` | VPC, subnets, routes, gateways, endpoints, security groups, NACLs |
| `velmakina-view-compute` | instances, volumes, launch templates, load balancers, Auto Scaling |
| `velmakina-view-stacks` | CloudFormation lists; stack details for `velmakina-*`; `tag:GetResources` |
| `velmakina-view-monitoring` | CloudWatch alarms and metrics, log group names |
| `velmakina-view-s3` | bucket list; settings of `velmakina-*` buckets |
| `velmakina-view-iam` | role and policy lists; documents of `velmakina-*` roles and policies |
| `velmakina-deny-data` | unchanged: explicit Deny on data and secret reads |

The role attaches all seven. Same actions and scoping as before; only the grouping changes.

**Runner-up:** option 1. Reconsider if the policy count gets close to the role limit.

## Accepted downsides

- Seven policies to name and maintain instead of two.
- Close to the default limit of 10 managed policies per role.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| An action lands in the wrong group or is lost in the split | Low | Simulator check of the whole set after the split |

## Related

- Implemented in: `infra/iam-readonly.yaml`.
- Builds on: ADR-0006 (Deny, trust), ADR-0007 (named actions).
