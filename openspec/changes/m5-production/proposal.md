# Proposal

> **Approved** by the author on 2026-11-28 ("Accept and commit"), with the AI-review
> verdicts. Written by Claude Code on 2026-11-27 from the brief's Milestone 5,
> ADRs 0011–0031 and the M4 lab findings (`docs/m4/evidence/aws-deploy.txt`, "What the
> lab found, and where it went"); revised on 2026-11-28 after the AI design review
> (`docs/m5/ai-review.md`) with the verdicts the author confirmed ("Yes, apply them").
> The decisions are in ADRs 0032–0036, all **Accepted**.

## Why

M1–M4 built a store that survives a lost replica and shows what it is doing, but its size
is fixed: two replica sets on three nodes, whatever the load. M5 makes it grow and shrink
on the signal M4 built (p99 per replica set from the monitor's Prometheus), moves keys
when a replica set joins or leaves without taking reads down or losing writes, and adds an
ops agent that reads the same alerts and metrics and proposes what to do, with its
reasons. The final benchmark shows how the whole system behaves under gradual, spiking and
sustained load.

## What Changes

- **Autoscaling on p99, not CPU.** Scale-out conditions are Prometheus rules on the
  coordinator's p99 per shard, confirmed by the shard's own latency (writes: > 50 ms with
  the leader > 40 ms for 3 minutes; reads: > 12 ms with a replica > 8 ms for 5 minutes; at
  ≥ 100 requests/s); scale-in after 10 minutes below and once the load that caused the
  scale-out has gone. A small executor run by the author applies them, one action at a
  time, with bounds (2–3 replica sets), cooldowns, and no scaling during a failure, a
  migration, or just after a write hold. ADR-0032, ADR-0035.
- **The scaling unit is a replica set.** The two base replica sets stay on nodes a/b/c;
  the extra replica set is three single-subnet Auto Scaling groups (one instance each, one
  per AZ) that never act on their own; the executor sets their capacity. The coordinator
  moves to t3.medium, since it saturates first. ADR-0035.
- **Shard addition and removal with background migration.** A new replica set registers
  as *pending* and is not on the ring until its keys have moved. A migration copies only
  the keys whose owner changes, from each source leader's committed state and then its
  committed log tail; the old shard keeps serving reads and writes for those keys until a
  cutover fenced on the source itself by a replicated freeze record; afterwards the source
  purges the moved keys. Removal is the same in reverse, for the extra set. Migration
  state survives a coordinator restart. ADR-0033.
- **Before/after shard sizes** from the M4 dashboard (`kv_store_keys`, `kv_log_bytes`,
  requests per shard) for every join and leave, with a verifier's result, recorded in
  `docs/m5/rebalancing.md`.
- **Ops agent (required) with an approval gate (bonus).** It reads alerts, a fixed set of
  metric queries and cluster state through read-only tools, and returns one typed proposal
  (transfer a leader, abort a stalled migration, scale up or down when the autoscaler
  can't, or nothing) with its reasoning and the numbers it used. It runs without AWS
  credentials or an admin key; a proposal runs only after the author approves it in a
  separate command, which re-checks every guard. ADR-0034.
- **Leader placement.** The coordinator waits (up to 3 minutes) for all replicas before
  choosing the first leaders and prefers a node without a leader when choosing; a leader
  moves on purpose only through a fenced, approved transfer (the M4 lab saw both leaders on
  one node twice). ADR-0036.
- **Health and version endpoints.** `/health` stays; `/health/live`, `/health/ready` and
  `/version` (commit, build time, Go version, role) are added on the coordinator and every
  replica, and a `kv_build_info` metric shows which release each process runs.
- **Final benchmark** with autoscaling on: gradual increase, sudden spike and sustained
  high load, with latency, throughput and active replica sets and nodes over time, plus
  unthrottled workloads A and B on 2 and 3 sets to compare with the M2 baseline.
- **Monitor sized for long load tests:** a larger data volume and Loki retention, because
  Loki grew ≈ 0.8 GB per hour on an 8 GB disk in M4.
- **Breaking (internal):** a shard that registers is no longer added to the ring at once
  (supersedes in part ADR-0013); record format 3 (freeze and purge records; supersedes in
  part ADR-0022), which M4 binaries refuse; the latency histograms gain a 12 ms edge
  (supersedes in part ADR-0031).

## Capabilities

### New Capabilities

- `autoscaling`: scaling conditions on p99, guards, bounds, cooldowns, the executor, the
  scaling unit and how new replica sets join.
- `shard-rebalancing`: pending shards, ring changes, key migration, fenced cutover, purge,
  removal, progress and evidence.
- `ops-agent`: the agent's inputs, its proposals, the action set, the approval gate, its
  limits and its evaluation.
- `leader-placement`: one leader per node where possible, fenced planned transfer.
- `health-endpoints`: liveness, readiness and build version on every process.
- `load-testing`: the final benchmark profiles, the comparison runs, what is recorded, and
  the monitor's capacity for them.

### Modified Capabilities

None in `openspec/specs/` (it holds only M1's capabilities). M2–M4's capabilities
(`kv-api`, `replication`, `observability`, …) aren't archived yet, so where M5 changes
their behaviour (the ring, the record format, the histogram buckets) it is written as new
requirements in the capabilities above, as M4 did.

## Non-Goals

- Vertical scaling of the nodes as the scaling action: the write limit is a lock held
  across `fsync` per leader, which a bigger instance doesn't move (ADR-0035).
- More replicas per set (N > 3), a second coordinator, or removing a base set.
- Group commit and hedged reads (deferred from M3): changing fsync batching or the read
  path while measuring autoscaling would change two things at once. Migration batches its
  own writes; client writes don't change.
- The agent acting without approval, touching data (items), or moving a leader on its own.
- Scaling on a schedule, predictive scaling, or more than three replica sets.
- Raising the availability SLO back to 99.5% (ADR-0030's note); only after a drill measures
  a one-leader node loss.

## Impact

- Code: `internal/ring` (two rings during a migration), `internal/coordinator` (pending
  shards, migrations, cutover, placement, step-down and epoch push, admin API, scaling
  state, `/version`), `internal/shard` (state at a commit point, committed filtered log
  reads, batch apply with given stamps, freeze, step-down), `internal/store` (record
  format 3), `internal/metrics` (12 ms edge, build info, migration metrics), new
  `cmd/kvscale` (executor), new `cmd/kvops` (agent), `api/openapi.yaml` (admin routes),
  `api/internal-openapi.yaml`.
- Infra: `infra/app.yaml` gets three single-subnet Auto Scaling groups and a launch
  template for the extra replica set (capacity not in the template), a t3.medium
  coordinator, a larger monitor volume; Prometheus finds replicas through the
  coordinator's service-discovery endpoint instead of a fixed list.
- Monitoring: scaling rules and their tests, `LeadersColocated` and `MigrationStalled`
  alerts, dashboard panels for active replica sets and nodes, migration progress and write
  holds.
- Who runs what: the author runs deploys, deletes and the executor (Claude Code's auto-mode
  checker refuses `make up-app`/`make down-app`, and the executor needs the author's lab
  credentials); Claude Code runs local tests and read-only checks.
- AWS: up to three more t3.small while the extra replica set is up (≈ $0.06/h), a bigger
  coordinator (≈ +$0.02/h); ≈ $29.5 of the credit is left, runway to about 2026-12-20
  before M5, ≈ 2026-12-18 after its estimated ≈ $3.4.
