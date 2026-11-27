# ADR-0035: The scaling unit, and how scaling runs in the sandbox

- Status: **Accepted** — 2026-11-28 by Iryna Velmakina (author) in chat ("Accept and
  commit"), with the AI-review verdicts she confirmed ("Yes, apply them"). Drafted with
  Claude Code; revised after the AI design review (`docs/m5/ai-review.md`).
- Date: 2026-11-27
- Settles ADR-0014's "M5 will replace the shard instances with an Auto Scaling group"
  (only the extra replica set gets groups; nodes a/b/c stay) and ADR-0020's "the scaling
  unit is a replica set". Changes ADR-0014's coordinator size: t3.small → t3.medium.

## Context

ADR-0032 says *when* to scale. This ADR says *what* is added and *who* does it.

- **Topology:** nodes a/b/c, one per AZ, each running one replica of shard-1 and one of
  shard-2; N = 3, W = 2, R = 2 (ADR-0020, ADR-0021). With the coordinator, the monitor and
  the benchmark client: 6 × t3.small.
- **Bottlenecks measured in M2** (`docs/m2/benchmark.md`): writes are limited per leader
  by one lock held across `fsync`, not by CPU; the coordinator is "the next bottleneck"
  (it handles every request twice and used up its CPU credits at ≈ 1,800 ops/s).
- **Sandbox** (`docs/m1/labrole-dissection.md`, `quotas.md`): `voclabs` may create launch
  templates, Auto Scaling groups and scaling policies, and CloudWatch alarms; **CloudWatch
  custom metrics are denied**, so our p99 can't reach a CloudWatch alarm. Instances
  (`Inventory-App-Role`) **can't change Auto Scaling capacity** or launch instances. We
  can't create IAM roles, so a Lambda has no role to run with. Only t3.small and t3.medium
  are allowed. The lab's instance cap is unknown; we have run 6 at once.
- **An Auto Scaling group acts on its own even without policies:** `AZRebalance` moves
  instances between AZs (terminating one), `ReplaceUnhealthy` replaces an instance that
  fails health checks with an empty one (AI review W5).
- **Credentials** are the author's lab session credentials, refreshed each session and
  never written anywhere (CLAUDE.md). Claude Code's auto-mode checker refuses `make up-app`
  and `make down-app`, so the author runs AWS writes.
- **Budget:** ≈ $29.5 left, runway to about 2026-12-20 at ≈ $1.25 a day (the lab's NAT and
  our secrets); our own costs are instance hours and cross-AZ traffic (`docs/m4/cost.md`).

## Options — what is added

| | **1. A replica set (a new shard) on 3 new instances, one per AZ** | 2. A 4th replica in each existing set | 3. Vertical: t3.small → t3.medium for the nodes |
| --- | --- | --- | --- |
| Adds write capacity | **yes: a third leader, a third lock, takes a third of the writes** | no: writes still go to one leader per shard | no: same 2 vCPU and the same EBS `fsync`, so the per-leader lock limit doesn't move |
| Adds read capacity | yes | yes (R = 2 over 4) | a little (more memory, higher CPU baseline) |
| Needs | rebalancing (ADR-0033) | a 4th AZ or two replicas in one AZ | a stop/start per node: a leader transfer each |
| Instances while scaled out | 9 | 8 | 6 |
| Extra cost while scaled out | 3 × $0.0208/h ≈ $0.06/h | ≈ $0.04/h | ≈ $0.06/h |

## Options — how the extra set is launched

| | A. One group over three subnets, desired 0 ↔ 3 | **B. Three single-subnet groups (`-3a`, `-3b`, `-3c`), max 1 each** | C. Plain `RunInstances` from a launch template |
| --- | --- | --- | --- |
| Replica ID fixed | no: AZ placement can put two in one AZ → duplicate IDs | **yes: by the group's subnet** | yes: by a tag |
| Terminates on its own | `AZRebalance`, `ReplaceUnhealthy` | **no, with both processes suspended and scale-in protection** | no |
| Cleaned up with the stack | yes | **yes** | no: instances outside CloudFormation |

## Options — who executes

| | A. ASG step policy on a CloudWatch alarm | **B. Our rules in Prometheus + an executor run by the author, setting the groups' desired capacity** | C. Same executor, changing a CloudFormation parameter | D. Executor in AWS (Lambda, or on the monitor) |
| --- | --- | --- | --- | --- |
| Metric it can use | only AWS's own: an ALB's p99 for the whole store (no per-shard p99; ALB ≈ $0.02/h + LCUs) | p99 per shard from Prometheus | same | same |
| Can it run in the sandbox | yes, with the wrong metric | **yes** | yes | **no**: Lambda needs a role we can't create; instances can't change capacity |
| Knows the order (migrate before terminate) | no | **yes** | yes | — |
| Capacity drifts from the template | — | **not if the template doesn't set it** | never | — |
| Runs when the laptop is off | yes | **no** | no | — |

## Decision

**Unit: option 1. Launch: option B. Execution: option B, keeping C's property.**

- **Base capacity:** shard-1 and shard-2 stay on nodes a/b/c as now (tested in M3–M4).
- **The coordinator runs on t3.medium** (≈ +$0.02/h). It saturates first (M2), so it must
  not be the resource that limits the p99 the scaling acts on (AI review W3). Its CPU and
  credits are a stop condition in calibration (ADR-0032).
- **The extra replica set** is three Auto Scaling groups, `velmakina-kv-shard-3a/-3b/-3c`,
  each in one private subnet, min 0, max 1, from one launch template running the same
  release. The replica ID (`3a`, `3b`, `3c`) and the shard ID come from the group's tags.
  `AZRebalance` and `ReplaceUnhealthy` are **suspended** and new instances are **protected
  from scale-in**, so a group never terminates or replaces on its own. No scaling
  policies. A failed shard-3 instance is handled like any failed replica: alerts, and a
  person (ADR-0023's `make rebuild-replica`).
- **Capacity is not in the template:** the groups' `DesiredCapacity` is left out, so a
  stack update never resets it; `make up-app` refuses to run while shard-3 isn't retired
  (it asks the coordinator).
- **Bounds:** 2–3 replica sets. A fourth needs 12 instances and isn't planned.
- **Executor `cmd/kvscale`**, run by the author in a terminal on the laptop during a lab
  session, with the `academy` profile:
  1. reads `ScaleOutDue` / `ScaleInLatencyOk` and the guard metrics from Prometheus
     (through SSM port forwarding, as `make prometheus` does), every 15 s;
  2. checks the guards (ADR-0032); the rate at the last scale-out and the end of the last
     action are read from the coordinator's migration record, not kept on the laptop;
  3. **scale-out:** desired 1 on each of the three groups → waits for shard-3 to be
     pending with a leader and ≥ 2 live replicas → starts the join migration through the
     coordinator's admin API → done when the migration is done;
  4. **scale-in:** starts the leave migration → waits until shard-3 is retired → removes
     scale-in protection and sets desired 0 → the coordinator forgets shard-3 after it
     stops heartbeating;
  5. logs every decision (blocked ones too) and posts it to Alertmanager, so it reaches
     Telegram;
  6. executes approved ops-agent proposals (ADR-0034) through the same steps and guards.
  The admin key comes from Secrets Manager at start (as `make smoke-aws` reads the ops
  key), held in memory only.
- **Who runs what:** the author runs `make up-app`, `make down-app` and `make autoscale`;
  Claude Code runs local tests, `promtool`, `cfn-lint` and read-only AWS checks.
- **Prometheus finds replicas** through an HTTP service-discovery endpoint on the
  coordinator's metrics port (it knows every replica's address from heartbeats), so new
  instances are scraped without editing a target list, locally and in AWS.

**Why not vertical for the nodes:** a t3.medium has the same 2 vCPUs and the same EBS, and
the write limit is a lock held across `fsync` per leader (M2), so it wouldn't move. A third
leader does. Vertical pays where CPU is the limit: the coordinator, hence t3.medium there.
The per-shard multiplier would be group commit, deferred again (proposal, non-goals).

**Runner-up:** launch option C (plain `RunInstances` with replica-ID tags), if single-subnet
groups misbehave; execution option C (a CloudFormation parameter), if drift turns out to
matter despite the template leaving capacity out.

## Accepted downsides

- **Autoscaling works only while the author's executor runs.** In this sandbox nothing in
  AWS can hold the credentials to change capacity. A production deployment would run the
  executor with its own role; here the gap is stated, not hidden.
- Nodes a/b/c don't scale; only shard-3 comes and goes.
- A failed shard-3 instance isn't replaced automatically: a person rebuilds it, as for any
  replica.
- Three groups instead of one: more template, one per replica.
- The coordinator costs ≈ $0.02/h more for the whole session.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| The lab caps instances below 9 | Medium | A read-only check of the lab's limits before the first session; the first scale-out in calibration shows it early; fallback: a new ADR |
| The laptop sleeps or the SSM session drops mid-action | Medium | Each step is idempotent and re-read from the coordinator's state on restart; a migration continues without the executor |
| Credentials expire mid-session | Low | Sessions are ≤ 3 h and planned so a profile ends well before; the executor stops with a clear message and leaves the cluster as it is |
| A stack update while scaled out | Medium | Capacity isn't in the template; `make up-app` refuses while shard-3 isn't retired |
| Spend from leaving shard-3 up | Low | `make down-app` deletes the groups with the stack; desired 0 at the end of every test |

## Related

- ADR-0014 (app infrastructure), ADR-0020 (placement, scaling unit), ADR-0029 (monitor,
  SSM), ADR-0032 (thresholds, guards), ADR-0033 (migration), ADR-0034 (agent proposals).
