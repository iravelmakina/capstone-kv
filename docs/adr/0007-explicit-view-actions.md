# ADR-0007: Explicit view actions instead of wildcards

- Status: **Accepted** — 2026-11-06 by Iryna Velmakina (author). Drafted with Claude Code
  after comparing M1 with the lecturer's defense questions (02.11 recording).
- Date: 2026-11-06
- Supersedes: ADR-0006's line "`velmakina-view`: allows `Describe*`, `List*` and
  configuration-only `Get*` actions". The rest of ADR-0006 (separate Deny policy, MFA
  trust, no `:root`) stands.

## Context

ADR-0006 allowed action wildcards (`ec2:Describe*`, `lambda:List*`, …) and relied on the
Deny policy to remove the dangerous reads. Checking the policy found three wildcards that
quietly included a secret read, each closed by hand:

- `ec2:Describe*` → `DescribeInstanceAttribute`, `DescribeLaunchTemplateVersions` (user data)
- `autoscaling:Describe*` → `DescribeLaunchConfigurations` (user data)
- `lambda:List*` → `ListFunctions` (environment variables)

A wildcard also grows by itself: every new API AWS adds under that prefix is allowed
automatically. The lecturer's main question on policies is exactly this: what can
`Describe*` on `*` read?

## Options

| | 1. Wildcards + a growing Deny list (now) | 2. Every allowed action named | 3. AWS `ViewOnlyAccess` |
| --- | --- | --- | --- |
| A forgotten secret read | allowed until someone notices | **not allowed**: not on the list | depends on AWS |
| New AWS APIs | allowed automatically | allowed only when added | AWS decides |
| Length | ~20 lines | ~45 lines | one ARN |
| Explain every action at the defense | no: the wildcard hides them | **yes** | no (refuted in ADR-0006) |

## Decision

**Option 2.** `velmakina-view` names each allowed action, and only for services this
project uses: VPC and EC2 (network, instances, volumes, launch templates as a list),
load balancers, Auto Scaling, CloudFormation, CloudWatch alarms and metrics, log groups,
S3 bucket settings, IAM roles and policies, and `tag:GetResources`. Unused services
(Lambda, RDS, DynamoDB, SNS, SQS) are dropped.

`Resource` is `"*"` only for actions that don't support resource-level permissions;
everything else is limited to `velmakina-*` (stacks, roles, policies, buckets).

`velmakina-deny-data` stays unchanged as the safety net, in case a broader policy is
attached to the role later.

**Runner-up:** option 1. Reconsider only if the list becomes too costly to maintain.

## Accepted downsides

- A longer policy to read and maintain.
- A reviewer can't see services outside the project.
- A new need means adding an action (and an `AccessDenied` until then).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A listed action itself returns a secret | Medium | Checked per action when added; the Deny policy still covers the known ones; simulator test |
| A reviewer misses an action they need | Low | Fails safe (`AccessDenied`); add it |

## Related

- Implemented in: `infra/iam-readonly.yaml` (`velmakina-view`).
- Evidence: 72–73 (`simulator.txt`); `docs/m1/iam-readonly.md`.
