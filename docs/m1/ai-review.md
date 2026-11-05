# M1 AI design review

> The critique must come from a **fresh AI session with no project context**, not from
> the session that drafted the design (course rule; CLAUDE.md rule 5). This file holds the
> prompt, the transcript and my verdicts.

## How to run it

1. Open a new chat in any AI assistant (a new Claude.ai chat, a new Claude Code session
   in another folder, or another vendor's model).
2. Copy the design to the clipboard:
   ```bash
   cat openspec/changes/m1-infra-setup/proposal.md openspec/changes/m1-infra-setup/design.md openspec/changes/m1-infra-setup/specs/*/spec.md docs/adr/*.md docs/m1/labrole-dissection.md | pbcopy
   ```
3. Paste the prompt below, then the clipboard.
4. Save the full reply under **Transcript**, then fill in **My verdicts**.

## Prompt

```text
You are a senior cloud architect reviewing a student's design. Be critical: your job is
to find weaknesses, not to praise.

Context: a course project to build a small DynamoDB-like key-value store over five
milestones (sharding, replication with quorum reads, monitoring, autoscaling on p99).
This is milestone 1: AWS network and IAM foundation, built in a restricted AWS Academy
sandbox (no IAM role creation, instances limited to t3.medium, no ECS, no AWS CI/CD).

Review the documents below and give:
1. The 5 most serious weaknesses, ranked: what is wrong, why it matters for later
   milestones, and what you would do instead.
2. Any decision where a rejected alternative is actually better.
3. Anything missing that milestone 1 should cover.
Be specific and cite the document and section.

[documents follow]
```

## Transcript

- Model and tool: Claude Code (Opus 5.5), a **separate session started fresh** with no
  access to the drafting conversation, in read-only (plan) mode. It read only the files
  listed in the prompt. The prompt above was sent verbatim, minus the pasted documents,
  which it read from the repo instead.
- Date: 2026-11-05

### Independent AI design review — M1 (`m1-infra-setup`)

Reviewer: a fresh Claude Code session with no prior project context (Opus 5.5), 2026-11-05.
Read only: proposal.md, design.md, specs/*/spec.md, docs/adr/0001–0006, docs/m1/labrole-dissection.md,
and the "Milestone 1" section of docs/course/capstone-brief.md. Not read (out of scope): tasks.md,
budget.md, quotas.md, evidence/, the shared session notes. No files in the repo were changed and no AWS calls were made.

#### 1. The five most serious weaknesses (ranked)

##### W1. The no-egress decision (ADR-0005) doesn't match what the dissection found, so the private tier has no working way to run or operate anything
*ADR-0005 §Context, §Decision, §Runner-up, §Risks vs labrole-dissection.md §Inventory-App-Role and §What this means for later milestones.*

- **Interface endpoints are your planned fallback, but the role can't use them.** ADR-0005 makes interface endpoints the runner-up, "expected to be added for CloudWatch in M4", and puts their cost into the risk table. The dissection says instances "can't … send CloudWatch metrics or logs", and that `voclabs` has an implicit deny on custom metrics and dashboards. A `monitoring`/`logs` endpoint gives a node a route to an API it isn't allowed to call. That would be $7.30/AZ/month for nothing.
- **The DynamoDB gateway endpoint has no consumer.** The dissection lists "DynamoDB tables" under implicit deny. It costs nothing, but it shows the network was designed before the dissection was read.
- **Systems Manager is quietly lost.** The dissection lists Run Command and Session Manager as allowed, and the instance role carries `AmazonSSMManagedInstanceCore`. But the SSM agent needs the `ssm`, `ssmmessages` and `ec2messages` endpoints, or internet egress, and neither exists. So the one sanctioned way to run commands on a fleet won't work in the private subnets. The brief's M4/M5 headings ("MCP server", "Ops agent", "operational automation") will need some way to act on nodes, and EC2 Instance Connect is a human SSH path, not an automation API.
- **The artifact supply chain is undesigned.** "Packages and builds come from S3 only" (§Accepted downsides) covers AL2023 dnf at best. It doesn't cover your language runtime's packages, YCSB, Prometheus or Grafana binaries (which the dissection's M4 plan depends on, "no egress needed"), or any container image. Who stages them into S3, from where, and how they are versioned is left open, and that will be the first thing M2 trips over.
- **Two claims to check.** "Stops with the lab session" is listed against the NAT instance, but the shard nodes stop with the session too, so it doesn't separate the options. ADR-0005 also never mentions the cheapest middle ground (see §2).

**Why it matters later:** M2 bootstrap, M4 monitoring and the M5 ops agent all depend on this. The current ADR is likely to be superseded in M2 under time pressure.

**Instead:** reconcile ADR-0005 with the dissection now. Drop the DynamoDB endpoint, or justify it. Strike CloudWatch endpoints as the runner-up. Add an explicit artifact-staging design: a project bucket whose bucket policy is conditioned on `aws:SourceVpce` of your S3 gateway endpoint, filled by CI. Add a parameterised, default-off NAT (see §2) for bootstrap windows. Decide now whether SSM endpoints (≈$22/month in one AZ) or EC2 Instance Connect is the ops channel.

**Related risk:** the dissection's M3 workaround grants `Inventory-App-Role` S3 write access via a bucket policy. That role is the lab's shared instance role, so the AWS capstone's own instances most likely carry it too. Without an `aws:SourceVpce` or `aws:SourceVpc` condition, you'd also be granting write access to the lab's instances.

##### W2. The read-only policy's definition of "view" can't be enforced by the spec that's supposed to test it, and its deny-list has gaps
*ADR-0006 §Decision, §Risks; spec iam-readonly-access, Requirements "View-only policy", "Explicit deny on data and secrets", "Assumable role".*

- **The test can't tell viewing from reading data.** The spec's test is "every allowed action is List or Read". In IAM's access-level taxonomy, data reads (`s3:GetObject`, `dynamodb:GetItem`, `secretsmanager:GetSecretValue`) are also Read. So that scenario passes a policy that leaks every byte. The thing ADR-0006 cares about, configuration vs content, isn't tested by any automated scenario. The other scenarios check only six named actions.
- **Data-revealing actions survive the deny-list.** It enumerates the bad actions (badness-listing), which is never complete. Examples that get past it:
  - `s3:GetObjectVersion`; `s3:ListBucket`, which returns object keys, and in a KV store keys are data
  - `dynamodb:PartiQLSelect` and `dynamodb:GetRecords` (streams)
  - `secretsmanager:BatchGetSecretValue` (a 2023 API, which shows why hand-maintained lists drift)
  - `ec2:DescribeInstanceAttribute` (user data, a common home for secrets) under the `Describe*` wildcard
  - `logs:GetLogEvents`, `logs:FilterLogEvents`, `logs:StartQuery`
  - `lambda:GetFunction` and `lambda:GetFunctionConfiguration` (code URL, environment variables)
  - `cloudformation:GetTemplate`
  - `ec2:GetConsoleOutput`, `ec2:GetPasswordData`

  Some of these depend on exactly which `Get*` actions you allow, but `Describe*` and `List*` are wildcards, and that's where the leaks sit.
- **A Deny inside a reusable *managed* policy doesn't combine well.** Attach "view-only" to any principal that legitimately writes data, such as a developer group, and it silently breaks their `s3:GetObject`. "Survives a broader policy on the same role" (ADR-0006 options table) is both the selling point and the trap.
- **The trust policy is under-specified.**
  - The "principal parameter" has no pattern. Passing `arn:aws:iam::<acct>:root` turns option A into option C ("any identity in an account"), which ADR-0006 rejected.
  - The spec doesn't fix the condition operator. It must be `Bool`, not `BoolIfExists`, which passes when the key is absent.
  - The design doesn't say which principal type is expected. `aws:MultiFactorAuthPresent` is reliably present only for IAM-user-with-MFA calls, so a role or federated principal may never satisfy it. That would make the role "assumable" only in theory.

**Why it matters later:** this is a graded SEC-1 deliverable, and it's what an examiner will probe. The same pattern will come back if M2+ gets an operator or auditor role.

**Instead:** see §2 (ViewOnlyAccess as the base). Turn the spec into a real check: run IAM Access Analyzer `check-access-not-granted` against a list of data-plane actions (if the lab allows it), or expand the wildcards with a policy-expansion tool and assert against an explicit "data actions" list, not access levels. Put the Deny in the role's inline policy, or in a separately named policy, not in the view policy. Constrain the principal parameter with `AllowedPattern` (user or role ARN, not `:root`). State `Bool` plus `aws:MultiFactorAuthAge`, and set `MaxSessionDuration`.

##### W3. The exported "internal" security group makes the VPC a flat network, and the reason given for it is grading, not architecture
*design.md Decision 7; spec network-foundation "Internal security group".*

- **One group trusts the whole VPC.** Inbound from the whole VPC CIDR, with ports and protocols unconstrained by the spec (it only checks the source), means that anything in a public subnet can reach every port on every data node. That covers an internet-facing load balancer target, a NAT or build instance, or a load generator. It erases the public/private distinction the brief asks you to build.
- **It couples the app to the network stack.** It sits in the network stack and is exported. Any app-port change then means touching the network stack, or living with "allow everything" forever, which works against ADR-0003's own goal ("App deploys must not be able to break the network").
- **The rationale won't hold at defense.** "Rejected: no groups in M1 — … the networking skill is graded" is not an architectural reason. An examiner will notice.

**Why it matters later:** in M2–M3 you'll have a client or LB tier, a coordinator, storage nodes and replica-to-replica traffic. The defaults set now are the ones that ship.

**Instead:** use security-group references, not CIDRs: `lb-sg` → `coordinator-sg` on the app port → `storage-sg` on the storage port, plus a self-reference for replication, and `eice-sg` → 22. If you want something in M1, export *empty* role SGs from the network stack and put rules in app stacks with `AWS::EC2::SecurityGroupIngress`. That shows the skill better than a CIDR rule.

##### W4. The export design contradicts the "additive growth" promise and drops the AZ→subnet mapping M3 needs
*ADR-0003 §Accepted downsides, §Risks, §Related (export names); ADR-0004 §Future optimizations, §Risks.*

- **"Additive" isn't additive.** ADR-0003 exports `-PublicSubnetIds` and `-PrivateSubnetIds` as lists and admits "an export can't change while another stack imports it". ADR-0003's mitigation and ADR-0004's future optimisation both say adding a 4th AZ is "additive; replaces nothing". It isn't: a new subnet changes the list export's value, which CloudFormation refuses while any app stack imports it. So the one growth path the CIDR plan reserves space for requires tearing down every app stack.
- **No AZ mapping.** Placing one replica per AZ (M3) needs to know which subnet is in which AZ, and a joined list only carries that implicitly, through ordering. Nothing exports the AZ IDs.
- **The SSM option is dismissed on an overstated claim.** The rejection says "no: VPC can be deleted under the app". In practice EC2 refuses to delete a subnet, security group or VPC with live ENIs (DependencyViolation), so the "protection" of exports is mostly the lock itself.

**Instead:** export one value per subnet and per AZ (`-PrivateSubnetA/B/C`, `-AzIdA/B/C`). Adding AZ D is then a genuinely new export. Keep list exports only as a convenience, and never import them from long-lived stacks.

##### W5. The M2 compute assumption (t3.small, credit mode `standard`) undermines the p99 goal of M2–M5
*design.md Decision 6 and §Risks (last bullet).*

- **Standard mode throttles at sustained load.** t3.small and t3.medium have a 20%-per-vCPU baseline. With `standard` credits, a long YCSB run drains the balance and the instance is throttled to baseline: p99 latency falls off a cliff that has nothing to do with your sharding, replication or quorum design.
- **It corrupts M5.** Autoscaling on p99 will fire on credit exhaustion, not load. Newly launched nodes start with little or no credit, so scale-out can make p99 worse.
- **The "fix" in §Risks swaps a cost risk for a correctness-of-measurement risk.**

**Instead:** treat credits as a first-class variable in the M2 ADR. Either stay `unlimited` and budget the surplus (bounded by test duration), or use `standard` and gate every benchmark on `CPUCreditBalance` (an AWS-published metric, so available despite the custom-metric deny), reporting it alongside p99. Put the surplus scenario in the budget now.

#### 2. Decisions where a rejected (or ignored) alternative is better

- **ADR-0006, policy scope: option 2 (`ViewOnlyAccess`) is better as the base.** It is AWS's own maintained answer to exactly your definition of "view, not data", and it tracks new APIs, which your hand list won't (W2). The table scores "AWS changes it" as a downside; for this purpose it is the upside. Best of both: `ViewOnlyAccess` plus a small inline Deny on data-plane reads as defence in depth, with the role refusing anything beyond that.
- **Design Decision 7: the rejected "no SG in M1" is better than a CIDR-wide group.** SG-reference role groups are better than both (W3).
- **ADR-0005: a conditional NAT Gateway, never listed, beats both "gateway endpoints only" and the NAT instance.** A template parameter `EnableNat=false` by default, one NAT, one route per private table. It costs $0 idle, ≈$0.045/h only during bootstrap or AMI-build windows, solves pip, Docker and binary downloads, and needs no patching. The ADR's own "Future optimizations" admits it is one resource plus routes.
- **ADR-0004: a non-overlapping VPC CIDR (e.g. `10.20.0.0/16`) was never considered and costs nothing.** The ADR carefully avoids overlap with the default VPC, Docker and local Kubernetes, then accepts overlap with the one VPC that actually shares the account (the lab's "Project VPC"). That makes peering impossible and invites console and script confusion. Two smaller points in the same ADR:
  - "lower latency from Kyiv" is irrelevant unless clients run outside AWS, and they will likely be load generators in-region.
  - "us-west-2 is a parameter change" is false: AZ IDs are region-scoped (`use1-*`) and the t3 capacity evidence is us-east-1 only.
- **ADR-0002 (IaC tool): the decision stands, but the comparison is skewed.**
  - The Terraform row "half-applied state stays" is presented as worse than it is.
  - The CDK rejection ("bootstrap creates IAM roles → refused") cites no evidence. A custom bootstrap template, or the legacy synthesizer with no assets, avoids roles.
  - The "Future optimizations" note about CLI-credentials mode needing "no bootstrap roles" is only half true: that synthesizer still needs a bootstrap staging bucket.

  Expect this to be probed at defense.

#### 3. What M1 should cover per the brief but doesn't (or only on paper)

- **Budget vs sandbox credit ceiling (brief task 7).** The ceiling is still an open question (design.md §Open Questions), so the comparison the brief asks for can't be complete yet. I didn't read budget.md, so I can't check whether it was resolved there.
- **Quotas that can limit M2 (brief task 6).** ADR-0005 §Context says the lab's instance limit "isn't known yet". That is the quota most likely to bite: M3 needs 8 instances, and the lab's own resources share the account. Also worth listing: the vCPU On-Demand quota, EIPs/ENIs, and the EC2 Instance Connect Endpoint per-VPC limit. quotas.md was out of scope for this review.
- **"How you found out" (brief task 1, control question).** The console method is a placeholder ("author: do this and add screenshots", labrole-dissection.md §How we found out, row 5). Subnets and route tables, the core of the network stack, "weren't probed separately" (§voclabs Allows). The dissection is also explicitly an AI draft "to rewrite in own words".
- **Change sets and drift (control question): claimed, not delivered.**
  - The infra-lifecycle spec says updates "SHALL go through a reviewed change set" and the change set "is shown before it runs". But `make up` wraps `aws cloudformation deploy`, which creates *and executes* the change set without showing its contents. Nothing forces `make diff` first. Use `--no-execute-changeset`, print `describe-change-set`, then require an explicit execute.
  - Drift detection can't see a manually added `0.0.0.0/0` route, because out-of-band routes aren't stack resources and `AWS::EC2::RouteTable` has no routes property. So the defining invariant of "private = internal-only" isn't guarded by your drift check. Add a `make verify` that asserts the route tables directly.
  - The spec's status list also omits `NOT_CHECKED`.
- **Recreatability (brief task 8).** The rebuild is specified for a stateless network stack, where it is close to trivial. Nothing yet says how stateful pieces will behave on teardown: EBS shard volumes, non-empty S3 buckets, `DeletionPolicy` and snapshots. Worth one line in ADR-0003 so M2 doesn't discover it.
- **ADR status and scope.** All six ADRs are still `Proposed`, while the brief and the repo's own rules require accepted specs before IaC. ADR-0001 is about lab 1 multi-tenant buckets, not this milestone. Its *Decision* is AI-written with "(Author: confirm this was your lab1 conclusion)" and a TODO to paste the task statement. Presenting it in M1 as the author's decision is an integrity and defense risk: complete it from your actual lab 1 work, or drop it from the M1 set.
- **Goal contradiction.** design.md's goal "a network M2–M5 can use unchanged" conflicts with ADR-0005, which adds an EC2 Instance Connect Endpoint in M2 and interface endpoints in M4. Say which stack owns those. If the endpoint lives in an app stack, tearing that stack down also removes your admin access.

## My verdicts

Proposed by the drafting session; agreed by the author in chat on 2026-11-05.
*(Author: rewrite the arguments in your own words before the defense.)*

| # | AI point | Verdict | Argument | Change made |
| --- | --- | --- | --- | --- |
| 1 | W1 + §2: the private tier can't install or operate anything; use a NAT Gateway that's off by default | **Accept** | Right: interface endpoints for CloudWatch are useless when the instance role can't call CloudWatch, and nothing explained how YCSB, Prometheus or runtime packages reach the nodes. A switchable NAT costs $0 idle and ~$0.05/h only during installs | ADR-0005: CloudWatch endpoints and the DynamoDB endpoint dropped; M1 keeps no route out (nothing runs yet); the switchable NAT is the planned M2 path, decided in M2's ADR |
| 2 | §2: use AWS `ViewOnlyAccess` as the base of the read-only policy | **Refute** | The task says to *create* the policy, and every action in it must be explainable at the defense. A managed policy that AWS changes over time would silently widen an auditor's access, which is the opposite of least privilege. Its real concern, gaps in a hand-made list, is fixed differently: a separate Deny policy and a test against an explicit list of data actions | ADR-0006 keeps our own policy, moves the Deny into a second policy, extends the Deny list, tightens trust (`Bool`, MFA age, no `:root`) |
| 3 | §2: "lower latency from Kyiv" is irrelevant to the region choice | **Refute** | Some clients do run from Kyiv: the M4 MCP client (an AI assistant on my laptop), the M5 ops agent, and my interactive testing. Benchmarks will run in-region, but these won't | ADR-0004 now states which clients the latency matters for |

**Other points accepted without debate:** W3 (one VPC-wide security group made the
network flat → removed from M1; M2's app stack creates role groups that reference each other), W4 (list exports
broke "additive" growth → one export per value, with AZ IDs), W5 (`standard` CPU credits
would throttle benchmarks → stay `unlimited`, budget the surplus), the overlapping CIDR
(→ `10.20.0.0/16`), "us-west-2 is a parameter change" (corrected), change sets shown
before executing, a `make verify` route check, `NOT_CHECKED` in drift results, a teardown
note for stateful resources, and ADR-0001 (lab 1) removed from this repo.
