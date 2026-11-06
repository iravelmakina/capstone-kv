# Read-only policy and assumable role

> **Draft** — drafted with Claude Code; author to review and rewrite in own words.

Template: [`infra/iam-readonly.yaml`](../../infra/iam-readonly.yaml). Decisions: ADR-0006, ADR-0007.
**Never deployed** — the sandbox refuses it, which is part of the task.

## What it would do

| Resource | What it does |
| --- | --- |
| `velmakina-view` (managed policy) | Lets the holder see what exists and how it's configured, for the services this project uses: VPC and EC2, load balancers, Auto Scaling, CloudFormation, CloudWatch, log groups, S3 bucket settings, IAM. **Every action is named, no wildcards** (ADR-0007). Calls AWS accepts only on `"*"` are in one statement; stacks, bucket settings, roles and policies are limited to `velmakina-*`. No write actions. |
| `velmakina-deny-data` (managed policy) | Explicitly denies reading stored data and secrets: S3 object contents and keys, DynamoDB items and streams, secret values, SSM parameters, KMS decrypt, log contents, Lambda code, templates, user data (instances, launch templates, launch configurations), console output. An explicit Deny beats any Allow, so this holds even if a broader policy is attached later. |
| `velmakina-readonly` (role) | Has both policies. Only the IAM user or role given as `TrustedPrincipalArn` can assume it, and only with MFA used in the last hour. Sessions last at most 1 hour. A whole-account principal (`…:root`) is rejected by the parameter pattern. |

Example: an auditor in another account signs in with MFA, calls `sts:AssumeRole` on
`velmakina-readonly`, and gets temporary credentials that can list every VPC and bucket and read our project's
settings, but can't read a single object, item, secret or log line, and can't change anything.

## How it was checked

| Check | Result | Evidence |
| --- | --- | --- |
| `cfn-lint` | no findings | 71 (`dry-runs.txt`) |
| CloudFormation `validate-template` | valid; needs `CAPABILITY_NAMED_IAM` because the policies and role have fixed names | 71 |
| `simulate-custom-policy` with both policies | 17 data/secret reads → `explicitDeny`; 6 view actions → `allowed`; 4 writes → `implicitDeny` | 70 (`simulator.txt`) |
| Same, after two fixes from the lecture review: user data in launch templates/configurations denied, view policy scoped to `velmakina-*` | user-data reads → `explicitDeny`; our bucket, role, policy, table → `allowed`, someone else's → `implicitDeny`; account-wide lists `allowed`; data reads on our own resources `explicitDeny`; writes `implicitDeny` | 72–73 |
| Same, after naming every action (ADR-0007) | *to run when lab credentials are refreshed* | — |

## No wildcards, and where `Resource: "*"` remains

**Actions:** every allowed action is named (ADR-0007). An earlier version used wildcards
such as `ec2:Describe*`, and three of them quietly included secret reads: instance and
launch-template **user data** (startup scripts that may hold keys), launch configurations,
and Lambda environment variables. With a named list, an action that isn't written down
isn't allowed, including any API AWS adds later.

**Resources:** `"*"` means "any resource in the account". It remains only where AWS gives
no choice:

| Statement | Resource | Why |
| --- | --- | --- |
| `ListAndDescribeAccountWide` | `"*"` | calls such as `ec2:DescribeVpcs`, `s3:ListAllMyBuckets`, `cloudformation:ListStacks` don't support resource-level permissions: AWS accepts only `"*"`. A tag condition doesn't help either: a list call has no single resource to read a tag from |
| `ProjectStacks` | `…:stack/velmakina-*/*` | stack details, events and drift accept a stack ARN |
| `ProjectBucketSettings` | `arn:aws:s3:::velmakina-*` | bucket settings accept a bucket ARN |
| `ProjectRolesAndPolicies` | `…:role/velmakina-*`, `…:policy/velmakina-*` | IAM reads accept role and policy ARNs |

**The Deny stays as a safety net.** `velmakina-deny-data` still explicitly denies the
known secret reads (user data, Lambda environment variables, console output, passwords)
and all data reads, so they stay blocked even if a broader policy is attached later.

## Why the sandbox refuses it

Creating the template needs `iam:CreatePolicy` and `iam:CreateRole`. The lab role
`voclabs` has neither. Real attempts (nothing was created):

> `… is not authorized to perform: iam:CreateRole on resource: …role/velmakina-probe-role
> because no identity-based policy allows the iam:CreateRole action`

> `… is not authorized to perform: iam:CreatePolicy on resource: policy velmakina-probe-policy
> because no identity-based policy allows the iam:CreatePolicy action`

(evidence 60–61, `identity-and-roles.txt`). "No identity-based policy allows" is an
**implicit** deny: nothing grants the action. An explicit Deny statement or an SCP would
have been named in the message. Deploying the stack would fail on the first IAM resource
and roll back.

Why the lab is built this way: anyone who can create roles and policies can grant
themselves any permission (create an admin role, then assume it). Blocking IAM creation is
what keeps the sandbox a sandbox.
