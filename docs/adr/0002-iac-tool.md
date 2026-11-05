# ADR-0002: Infrastructure-as-code tool

- Status: **Accepted** — 2026-11-05 by Iryna Velmakina (author). Drafted with Claude Code; covered by the independent AI design review, unchanged (`docs/m1/ai-review.md`).
- Date: 2026-11-05

## Context

All infrastructure must be recreatable from the repo, in a sandbox where IAM roles can't
be created and credentials rotate every session. The same tool will carry M2–M5.

## Options

| | CloudFormation | Terraform | CDK |
| --- | --- | --- | --- |
| State | kept by AWS | state file to store and not lose | kept by AWS (it generates CFN) |
| Sandbox fit | works | works, but the state file must survive sandbox resets | `cdk bootstrap` creates IAM roles → **refused** |
| Preview | change sets | `terraform plan` | `cdk diff` |
| Drift detection | built in | `plan -refresh-only` | built in |
| Failed deploy | automatic rollback | half-applied state stays | automatic rollback |
| Loops / reuse | weak | good | full language |

## Decision

**CloudFormation (YAML).** No state to lose, automatic rollback, and change sets and drift
detection are exactly what the control questions ask about.

**Runner-up:** Terraform. Reconsider if later milestones need the local Kubernetes setup
managed by the same tool.

## Accepted downsides

- Verbose templates with no loops.
- AWS-only.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Stack stuck in `DELETE_FAILED` / `ROLLBACK_FAILED` | Medium | Delete blocking resources, retry; record it in the recreate log |
| No pipeline role possible, so M2 CI/CD can't deploy unattended | Medium | Decided in an M2 ADR |

## Future optimizations

- Move app stacks to CDK (using its CLI-credentials mode, which needs no bootstrap roles)
  if M2+ ends up with several near-identical stacks.

## Related

- Implemented in: `infra/*.yaml`, `Makefile`.
