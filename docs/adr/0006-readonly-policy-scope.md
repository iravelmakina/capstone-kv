# ADR-0006: Read-only policy scope and role trust

- Status: **Accepted** — 2026-11-05 by Iryna Velmakina (author). Drafted with Claude Code; revised after an independent AI design review (`docs/m1/ai-review.md`).
- Date: 2026-11-05
- Superseded in part by ADR-0007: the view policy's action wildcards.

## Context

The task asks to **create** an IAM policy that "allows ONLY to view resources (no write
access)", and a role carrying it that another principal can assume. Two choices: what
"view" covers, and who may assume the role.

## Options — policy scope

| | 1. AWS `ReadOnlyAccess` | 2. AWS `ViewOnlyAccess` | 3. Our own view policy + separate Deny policy |
| --- | --- | --- | --- |
| Sees configuration | yes | yes | yes |
| Reads data (S3 objects, DynamoDB items, secrets, logs) | **yes** | no | no |
| Survives a broader policy on the same role | no | no | yes: explicit Deny wins |
| Can I explain every action at the defense? | no | no — a black box AWS changes | yes |
| Effort | one ARN | one ARN | write and test two policies |

## Options — role trust

| | A. A specific user/role + MFA | B. Principal + External ID | C. A whole account (`:root`) |
| --- | --- | --- | --- |
| Fits | a person, e.g. an auditor | a third-party service (confused-deputy problem) | convenience |
| Weak spot | MFA must reach the call | a shared secret, not proof of a person | anyone that account allows can assume it |

## Decision

**Policy: option 3, as two policies.**
- `velmakina-view`: allows `Describe*`, `List*` and configuration-only `Get*` actions.
- `velmakina-deny-data`: explicitly denies reading data and secrets, including
  `s3:GetObject*`, `s3:ListBucket` (object keys are data in a key-value store), DynamoDB
  `GetItem`/`BatchGetItem`/`Query`/`Scan`/`PartiQLSelect`/`GetRecords`,
  `secretsmanager:GetSecretValue`/`BatchGetSecretValue`, `ssm:GetParameter*`,
  `kms:Decrypt`, `logs:GetLogEvents`/`FilterLogEvents`/`StartQuery`, `lambda:GetFunction*`,
  `cloudformation:GetTemplate`, `ec2:DescribeInstanceAttribute` (user data),
  `ec2:GetConsoleOutput`, `ec2:GetPasswordData`.

Keeping the Deny separate means the view policy can be reused without silently breaking a
principal that legitimately reads data.

**Trust: option A.** The trusted principal is a template parameter restricted to an IAM
user or role ARN (`AllowedPattern` rejects `:root`). The condition is `Bool`
`aws:MultiFactorAuthPresent = true` (not `BoolIfExists`) plus `aws:MultiFactorAuthAge`
≤ 3600. The expected caller is an IAM user who signed in with MFA, the case where the MFA
key is reliably present. `MaxSessionDuration` is 1 hour.

**Rejected: `ViewOnlyAccess` as the base.** The task says to *create* the policy, and every
action in it must be explainable. A managed policy AWS changes over time would silently
widen an auditor's access.

**Runner-up:** option 2. Reconsider if maintaining the action lists costs more than it teaches.

## Accepted downsides

- Both action lists are maintained by hand as AWS adds APIs.
- Some real auditing tasks need data reads; this role can't do them.
- A role or federated caller may not carry the MFA key, so only MFA-signed-in IAM users fit.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A wildcard (`Describe*`, `List*`) still allows a data read | High | Test against an explicit list of data actions with `simulate-custom-policy` (task 5.2), not against access levels |
| A new AWS data API isn't in the Deny list | Medium | Accepted; review the list when the template is touched |

## Related

- Implemented in: `infra/iam-readonly.yaml` (two managed policies + role trust policy).
- Spec: `iam-readonly-access`. Never deployed (ADR-0003).
