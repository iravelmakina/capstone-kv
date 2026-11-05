# M1 control questions — short answers

> **Draft** — drafted with Claude Code as defense prep; author to rewrite in own words.
> Each answer points to where this project shows it.

## 1. \*aaS: compare cloud models, when to use which

| Model | You manage | Provider manages | Use when | Here |
| --- | --- | --- | --- | --- |
| IaaS | OS, runtime, app, data | hardware, network, virtualisation | you need control over the machine | EC2 nodes for shards |
| PaaS | app, data | OS and runtime too | you want to ship code, not run servers | (Elastic Beanstalk, App Runner — not used) |
| FaaS / serverless | function code | everything else, scales to zero | short, stateless, spiky work | Lambda — rejected for stateful shards |
| SaaS | only your usage | the whole application | a standard need already solved | GitHub Actions for CI/CD |

Moving down the table: less control and less ops work, more lock-in.

## 2. Regions, Availability Zones

- **Region:** an independent geographic area with its own copy of AWS services and
  control plane (us-east-1). Resources and quotas are regional.
- **AZ:** one or more isolated data centres inside a region, with separate power and
  network, a few ms apart. Spreading across AZs survives a data-centre failure.
- **AZ names vs AZ IDs:** `us-east-1a` maps to a different physical AZ per account;
  `use1-az1` doesn't. That's why ADR-0004 picks AZs by ID, and skips `use1-az3`
  (no t3.small).
- Here: 3 AZs so 3 replicas survive losing one AZ (M3 quorum).

## 3. IaC and its value

Infrastructure described in versioned files and created by a tool, not by clicking.
Value: **reproducible** (teardown and rebuild, task 8), **reviewable** (diffs, change
sets), **documented** (the template is the documentation), **consistent** across
environments, and **recoverable** after mistakes. Here: CloudFormation (ADR-0002).

## 4. Roles vs user groups for assigning policies

- **Group:** a set of IAM *users* in one account; members get the group's policies
  permanently, with long-lived credentials.
- **Role:** an identity that people, services or other accounts *assume* to get
  temporary credentials. No permanent keys; works across accounts and for AWS services.
- Use groups for your own staff's base permissions; use roles for services (EC2 →
  `Inventory-App-Role`), cross-account access (our read-only role, ADR-0006) and anything
  that should expire. The lab itself works through roles (`voclabs`).

## 5. VPC, CIDR, block calculations

- CIDR `/n`: the first n bits are fixed; 2^(32−n) addresses. `/16` = 65,536; `/20` = 4,096.
- AWS reserves 5 per subnet (network, router, DNS, future, broadcast) → 4,091 usable in a /20.
- `Fn::Cidr [10.20.0.0/16, 8, 12]` = eight /20 blocks, stepping 16 in the third octet:
  10.20.0.0, 10.20.16.0, … 10.20.112.0. Half the VPC is used; `10.20.128.0/17` is reserved
  (ADR-0004). `10.20` rather than `10.0` because the lab's own VPC already uses `10.0.0.0/16`.

## 6. Public and private subnets: how they work

A subnet's type is decided by its **route table**, not a setting:
- **Public:** route `0.0.0.0/0 → Internet Gateway`; instances also need a public IP.
- **Private:** no default route; traffic to the internet is dropped. Here, private
  subnets reach S3 through a **gateway endpoint** (a prefix-list route).
- A **NAT Gateway** (in a public subnet) lets private instances start outbound
  connections. M1 has none; M2 may add one, switched on only for installs (ADR-0005).

## 7. When to use Allow and Deny

- Everything is **implicitly denied** unless an Allow matches.
- **Allow** grants the baseline permissions.
- **Explicit Deny** always wins, even over an Allow from another policy. Use it for hard
  limits nobody may bypass: guardrails, sensitive data.
- Seen here: `ReadOnlyAccess` allows `iam:GetRole`, but `Pvoclabs` explicitly denies it on
  `voclabs`, so it fails. Our ADR-0006 policy denies data and secret reads for the same
  reason.

## 8. What does the LabRole allow and deny? How did you find out?

See [`labrole-dissection.md`](labrole-dissection.md). In short: "LabRole" here is
`voclabs` (us) and `Inventory-App-Role` (instances). Network, EC2 up to t3.medium,
CloudFormation and S3 are allowed; IAM creation, ECS, AWS CI/CD and CloudWatch custom
metrics are denied. Found by reading policy names, the policy simulator, dry-runs, the
wording of error messages, and real attempts for `iam:CreateRole`/`CreatePolicy`.

## 9. Change sets vs drift detection

- **Change set:** template vs the stack's recorded state → *what will change* if I deploy.
  Look for **Replacement**. Every deploy shows it first and waits for confirmation (`make diff`, `make up`).
- **Drift detection:** the stack's recorded state vs what actually exists → *what someone
  changed by hand*. Use before a deploy and after incidents (`make drift`).
- A change set can't see manual changes, so check drift first. Drift can't see routes
  added outside the stack either, so `make verify` checks the route tables directly.
  Demonstrated in task 6.4.
