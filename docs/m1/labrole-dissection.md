# "LabRole" dissection

> **Draft** — drafted with Claude Code from discovery evidence; author to review and
> rewrite in own words.

## What "LabRole" means here

The brief's "LabRole" is a generic name (confirmed by the lecturer). This AWS Academy lab
has no role called `LabRole` (evidence 02). Two roles play its part:

| Role | What it is | How we use it |
| --- | --- | --- |
| `voclabs` | The role our CLI and console session runs as | Its limits decide what we can build |
| `Inventory-App-Role` | The role that instances run with (instance profile) | What our EC2 nodes will be allowed to do |

## How we found out

Five methods, each with a limit:

| Method | What it showed | Its limit |
| --- | --- | --- |
| 1. Read the policies (`list-attached-role-policies`, `list-role-policies`, `get-role-policy`) | Policy **names** for `voclabs`; the full inline policy of `Inventory-App-Role` | Reading `voclabs`'s own documents is **explicitly denied** |
| 2. Policy simulator (`simulate-principal-policy`) | Allowed / implicit deny / explicit deny per action, without doing anything | It's a model. Without `--resource-arns` it tests against `*`, so permissions scoped to specific resources show as implicit deny (66). Results that matter were confirmed by method 3 |
| 3. Dry-run (`--dry-run`) | Whether a real EC2 request would succeed | Only EC2 actions support it |
| 4. The error message itself | Which layer refused: "no identity-based policy allows" (implicit deny), "explicit deny in an identity-based policy", or "explicit deny in a service control policy" | Only appears when something is denied |
| 5. Console (IAM → Roles → `voclabs`) | The role page doesn't open: "Access denied to iam:GetRole … an identity-based policy **explicitly denies** the action" (95). Same refusal as the CLI (02) | It calls the same IAM APIs, so it can't see more than method 1 |

## `voclabs` — what it allows and denies

Policies: AWS-managed `ReadOnlyAccess` + inline `Pvoclabs` (evidence 03–04).

**Allows** (simulator 08, 62; dry-runs 30–38, 41–42, 44, 64–65):
- Read almost everything (`ReadOnlyAccess`)
- Network: VPCs, Internet Gateways, security groups, gateway and interface endpoints,
  Instance Connect Endpoint, Elastic IPs, NAT Gateways, Route 53 hosted zones (subnets and
  route tables weren't probed separately; the first deploy confirmed them, evidence 80–88)
- Compute: t3.small and t3.medium with `Inventory-App-Role`, launch templates, EBS
  volumes and snapshots, Auto Scaling groups and scaling policies, load balancers
- Other: CloudFormation, S3 buckets and bucket policies, CloudWatch Logs and alarms,
  SNS topics, EventBridge rules, Lambda, Systems Manager (Run Command, Session Manager),
  Secrets Manager

**Denies**, by layer:

| Layer | Example | Evidence |
| --- | --- | --- |
| **Implicit deny** (nothing allows it) | IAM: create role/policy, OIDC provider, `PassRole` on any other role. ECS, ECR, SQS, Kinesis, DynamoDB tables, Cloud Map. CodePipeline, CodeBuild, CodeDeploy. CloudWatch custom metrics and dashboards, X-Ray | 08, 62 |
| **Explicit deny, identity policy** (named in the error) | Running t3.large or bigger; reading `voclabs` itself (`GetRole`, `GetRolePolicy`) even though `ReadOnlyAccess` allows it | 39–40, 02, 06 |
| **Explicit deny, layer not shown** (simulator only: identity policy or SCP) | Amazon MSK (Kafka), Amazon MQ, CodeConnections | 62 |
| **Explicit deny, service control policy** (organisation level, invisible from inside the account) | AWS Pricing API | 45, 51 |
| **Region restriction** | Any EC2 call in eu-central-1 (us-east-1 and us-west-2 work) | 11 |

The most useful example for the control question: `ReadOnlyAccess` **allows**
`iam:GetRole` on every role, yet `GetRole` on `voclabs` fails, because `Pvoclabs`
**explicitly denies** it, and an explicit deny always wins.

## `Inventory-App-Role` — what instances may do

- Trust: only `ec2.amazonaws.com` can assume it (53).
- Policies: `ReadOnlyAccess`, `AmazonSSMManagedInstanceCore`, and inline `secretsaccess`
  (create, read and update any Secrets Manager secret in the account) (09–10, 52).
- Instances **can** read S3 and describe EC2 and Auto Scaling. They **can't** write to
  S3, send CloudWatch metrics or logs, publish to SNS, or change Auto Scaling capacity (63).
- **Least-privilege gap:** our shard nodes need none of the Secrets Manager rights and
  far less than `ReadOnlyAccess`. We can't create a narrower role (implicit deny on
  `iam:CreateRole`), so we accept it and note it as a sandbox limitation. The M1 IAM
  template shows what we'd do instead.

## What this means for later milestones

| Milestone needs | Blocked | Way around it (to decide in that milestone's ADR) |
| --- | --- | --- |
| M2 CI/CD | CodePipeline, CodeBuild, CodeDeploy, OIDC provider | GitHub Actions using lab credentials stored as secrets, refreshed each session |
| M3 replication log | MSK, Amazon MQ, SQS, Kinesis; instances can't write to S3 | A log file on S3 with a **bucket policy** granting `Inventory-App-Role` write access only from our VPC (`aws:SourceVpc`, since the lab's own instances share the role; `s3:PutBucketPolicy` is allowed), or Kafka/RabbitMQ on our own EC2 |
| M4 metrics, dashboards, traces | CloudWatch custom metrics, dashboards, X-Ray | Prometheus and Grafana on EC2 inside the VPC (no egress needed); Datadog/Sentry would need egress (ADR-0005) |
| M5 scaling on p99 | Custom CloudWatch metrics; instances can't set capacity | A load balancer publishes response-time metrics (including p99) by itself, and CloudWatch alarms and scaling policies are allowed |
| Container images | ECR | Docker Hub (needs egress) or builds delivered through S3 |

## Why the sandbox refuses our IAM template

Implicit deny on `iam:CreateRole` and `iam:CreatePolicy`: predicted by the simulator (08),
confirmed by real attempts (60–61). Quotes and explanation: [`iam-readonly.md`](iam-readonly.md).
