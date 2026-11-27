# Design

> **Approved** by the author on 2026-11-28 ("Accept and commit"), with the AI-review
> verdicts. Written by Claude Code on 2026-11-27; revised on 2026-11-28 after
> the AI design review (`docs/m5/ai-review.md`) with the verdicts the author confirmed
> ("Yes, apply them"). Decisions with real alternatives are in ADRs
> 0032–0036 (all **Accepted**); this file covers how they fit together and the smaller
> choices.

## Context

- **Now (end of M4):** a coordinator routes each key by a consistent-hash ring (xxHash64,
  128 virtual nodes, ADR-0011) to one of two replica sets; each set has 3 replicas on
  nodes a/b/c (one per AZ), a single leader, W = 2 / R = 2, automatic promotion by the
  coordinator (ADRs 0020–0023). Records carry `(epoch, LSN)` and an HLC stamp (ADR-0028).
  The leader keeps a commit point apart from its last appended LSN; a follower's pull
  returns appended records and its next pull is the ack (ADR-0023). Replicas learn leaders
  and epochs from heartbeat answers every 5 s (ADR-0020). Metrics, logs, dashboard and
  alerts run on a `monitor` instance (ADRs 0025, 0029–0031). API keys with scopes protect
  the public API (ADR-0027).
- **Already there for M5:** `GET /health` on the coordinator (200/503 by readiness,
  `version`, 503/504 counts) and on every replica (role, epoch, LSN, lag, `version`);
  `internal/buildinfo.Version` set by the linker; `kv_store_keys`, `kv_log_bytes`,
  requests per shard on the dashboard ("shard distribution"); `DELETE /internal/v1/shards/{id}`.
  Missing: a build-version endpoint, liveness apart from readiness, a ring that can change
  with data in it, a way to move a leader on purpose, any scaling.
- **Measured limits (M2):** writes are limited per leader by one lock held across `fsync`;
  the coordinator is the next bottleneck (CPU credits used up at ≈ 1,800 ops/s).
- **Sandbox:** no custom CloudWatch metrics, no IAM roles, instances can't change capacity,
  t3.small/medium only, instance cap unknown (we have run 6). Lab sessions ≈ 3 h;
  credentials expire with them.
- **Money:** ≈ $29.5 left, ≈ $1.25 a day of fixed spend (lab NAT, our secrets), runway to
  about 2026-12-20 before M5's own use (`docs/m4/cost.md`).

## Goals / Non-Goals

**Goals:** add and remove a replica set on p99 with no lost acknowledged write, nothing
uncommitted coming back, and no read outage; before/after shard sizes; an agent whose
proposals are useful and can't do harm on their own; liveness, readiness and version on
every process; a final benchmark that explains the system's behaviour against M2.

**Non-Goals** (beyond the proposal's): scaling the coordinator or the monitor at run time;
more than three replica sets; removing a base set; changing N, W or R; compacting logs
(purge frees memory, not disk); autoscaling with the laptop off (ADR-0035 says why);
moving a healthy leader without approval.

## Decisions

1. **Scaling signal and thresholds:** writes: coordinator p99 > 50 ms and leader p99 >
   40 ms for 3 min; reads: coordinator p99 > 12 ms and a replica's p99 > 8 ms for 5 min;
   ≥ 100 req/s per shard. Scale in after 10 min below with the load under
   `0.75 × (n−1)/n` of the scale-out rate. Guards (incl. 5 min after any write hold),
   10-min cooldown, 2–3 sets; scaling state in the coordinator; 12 ms becomes a bucket
   edge; calibrated first, stopping if the coordinator saturates. ADR-0032.
2. **Rebalancing:** pending shards; copy current state at the source's commit point, then
   its committed, filtered log tail; per-source cutover fenced by a replicated freeze
   record on the source; ≤ 2 s write hold on moving keys; purge records; record format 3;
   one migration at a time, persisted; only extra sets leave. ADR-0033.
3. **Agent autonomy:** proposes one typed action (`transfer_leader`, `abort_migration`,
   `scale_up`/`scale_down` only when the autoscaler is off or blocked, `no_action`) with
   reasoning; read-only tools; `status` key only, in a container without AWS credentials;
   executed only after the author approves that proposal; guards re-checked. ADR-0034.
4. **Scaling unit and execution:** an extra replica set = three single-subnet Auto
   Scaling groups (max 1 each, AWS's own rebalancing and replacement suspended, scale-in
   protection), capacity not in the template; executor `cmd/kvscale` run by the author
   with lab credentials; coordinator on t3.medium. ADR-0035.
5. **Leader placement:** bounded wait (3 min) for all replicas before the first leader;
   tie-break toward leaderless nodes; planned transfer fenced at the leader (step-down,
   synchronous epoch push), only on request. ADR-0036.
6. **Agent runtime** (below): Go program on the Messages API with the Go SDK's tool runner,
   model `claude-opus-5-5`. Needs the author's confirmation (an Anthropic API key).
7. **Health and version endpoints** (below): `/health/live`, `/health/ready`, `/version`,
   `kv_build_info`; `/health` unchanged.
8. **Monitor capacity** (below): 30 GB gp3 data volume, Loki retention 12 h, a disk check.

### Where the M4 findings went

| Finding (M4 lab) | Decision | Where |
| --- | --- | --- |
| (a) Both leaders start on one node when a node registers late (twice); ADR-0030 deferred placement to M5 | Bounded wait at first assignment, tie-break, approved transfers | ADR-0036, spec `leader-placement` |
| (b) Loki grows ≈ 0.8 GB/h on an 8 GB monitor disk; ADR-0029 rated it Low | Medium for M5: bigger volume, retention, disk check in the lab script | this design, "Monitor capacity"; spec `load-testing` |
| (c) p99 per replica set comes from ADR-0031's buckets | All thresholds on edges (50, 40, 8 ms exist; 12 ms added) | ADR-0032 |
| (d) ≈ $29.5 left, runway to ≈ 2026-12-20; instance hours and cross-AZ traffic | Max 3 sets; extra set only during tests; cost estimate below; stack deleted after each session | ADR-0035, "Cost" below |
| (e) Auto-mode checker refuses `make up-app`/`make down-app` | The author runs deploys, deletes and the executor; Claude Code runs local and read-only work | ADR-0035, tasks |

## Architecture

```
 laptop (the author)
 ├─ kvscale (executor) ──SSM port fwd──▶ Prometheus (rules: ScaleOutDue, ScaleInLatencyOk)
 │     │ academy profile ──▶ autoscaling:SetDesiredCapacity velmakina-kv-shard-3{a,b,c} (0 ↔ 1)
 │     └─ admin key ──▶ coordinator /v1/admin/... (migrations, leader transfer, cluster)
 ├─ kvops (agent, container: no ~/.aws, status key) ──▶ proposals/ (append only)
 └─ `kvops approve <id>` (outside the container) ──▶ approvals ──▶ kvscale

 coordinator (t3.medium): ring(s), pending/active/leaving/retired sets, migration state
   (incl. scaling state), write holds, placement, step-down and epoch push
   ├─ node a/b/c: shard-1 + shard-2 replicas (fixed)
   └─ ASGs velmakina-kv-shard-3a/3b/3c: one instance each, replica ID from the subnet
 monitor: Prometheus (http_sd from the coordinator), Alertmanager → Telegram, Grafana, Loki
```

### Scale-out, end to end

1. `ScaleOutDue` fires (ADR-0032); kvscale checks guards → desired 1 on the three groups.
2. Three instances boot (≈ 3 min), read their replica ID from the group's tags, start
   replicas `3a/3b/3c`, heartbeat. The coordinator holds shard-3 as **pending** and assigns
   its first leader (ADR-0036 rule).
3. kvscale sees shard-3 pending with a leader and ≥ 2 live replicas →
   `POST /v1/admin/migrations {"add":"shard-3"}` with the triggering rate.
4. The coordinator plans (`before` = {1,2}, `after` = {1,2,3}); 3's leader takes each
   source's commit point, copies state as of it, then tails the source's committed records
   (ADR-0033); per source: freeze record → target catches up → switch; then purge.
5. Migration done → the coordinator records the action's end; kvscale starts the 10-min
   cooldown and posts the result; the dashboard shows 3 sets, 6 nodes and the new key
   distribution.

Scale-in is the mirror: `{"remove":"shard-3"}` → copy to 1 and 2 → one cutover → purge on
3 → retired → protection removed, desired 0.

### Coordinator changes

- **Set states:** `pending` (registered, not on the ring), `active`, `leaving`, `retired`.
  ADR-0013's "unknown ID is added to the ring" goes; the base sets become `active` through
  the expected-shards setting at first start, as today, and can't leave.
- **Routing:** with no migration, one ring. During one, a key's owner is `before`'s until
  its source has cut over, then `after`'s. Writes to keys of a source in cutover wait on a
  hold (released by the switch, ≤ 2 s); a write the source refuses with 409 "moved" is
  retried on the target after the switch.
- **Leadership:** the first-leader wait (3 min, all expected replicas), the tie-break, and
  the planned transfer: a step-down call to the leader (it refuses writes, finishes or fails
  its in-flight commits, reports its final committed LSN), and a synchronous push of
  `(epoch, leader)` to every replica of the set (`POST /internal/v1/epoch`), acknowledged
  before writes are released. Heartbeats keep carrying the epoch for any replica the push
  missed.
- **Persistence:** migration and set states, plus the scaling state (rate at the last
  scale-out, end of the last action), in the coordinator's log next to leaders and tables.
- **Admin API** (public port, `admin` scope; `status` may only read the cluster state):
  `GET /v1/admin/cluster` (sets, states, leaders, nodes, ring, migration, scaling state),
  `POST /v1/admin/migrations`, `GET /v1/admin/migrations/current`,
  `DELETE /v1/admin/migrations/current` (abort before cutover),
  `POST /v1/admin/shards/{id}/leader`. In `api/openapi.yaml`.
- **Service discovery:** `GET /sd` on the metrics port: replicas' metrics addresses with
  `shard`, `replica`, `node` labels, for Prometheus `http_sd_configs`.

### Replica changes

- `GET /internal/v1/migrate/state?target=&ring=&upto=&after=&limit=` on the leader: current
  records (values and tombstones with stamps) of keys moving to `target`, sorted by key, as
  of commit point `upto` (taken once with `GET /internal/v1/migrate/commit` before the
  first page). The ring change is passed as the two shard lists, so the replica computes
  ownership with the same ring code.
- `GET /internal/v1/log?...&moving_to=&committed=true` — the pull with a key filter that
  serves only records at or below the leader's commit point.
- Applying copied records: an internal batch write with given stamps, replicated as
  normal records (one fsync per batch on each replica: batching for migration only).
- **Freeze** (`POST /internal/v1/migrate/freeze`, leader only): appends a freeze record for
  a ring change; from then on writes of its moving keys get 409 "moved"; an unfreeze record
  lifts it if the cutover is abandoned. A promoted leader rebuilds the frozen set from its
  log.
- **Step-down** (`POST /internal/v1/stepdown`, leader only) and **epoch push**
  (`POST /internal/v1/epoch`) for planned transfers.
- **Record format 3** adds *freeze* and *purge* records; purge records list at most 1,000
  keys each; replicas drop purged keys from memory without a tombstone. Format 2 records
  are still read; an M4 binary refuses a format 3 log.

### Metrics and alerts added

`kv_ring_shards`, `kv_shard_state{shard,state}`, `kv_nodes`, `kv_node_leaders{node}`,
`kv_migration_phase`, `kv_migration_records_copied_total{source}`,
`kv_migration_records_left{source}`, `kv_write_hold_seconds` (histogram),
`kv_build_info{version,role}`; latency buckets gain 0.012. The ops agent's named queries:
p99 and request rate per shard, leader-side and replica-side latency, errors, live
replicas, keys per shard, leaders per node, replication lag, migration progress, write
holds. Info-level alerts
`LeadersColocated` and `MigrationStalled` (no progress for 5 minutes). Dashboard: replica
sets, live replicas and nodes over time, migration progress, write holds, leaders per node,
versions running.

### Agent runtime

| | **Go program, Messages API + the Go SDK's tool runner** | Claude Code headless (`claude -p`) with a read-only MCP server | Managed Agents (Anthropic hosts the loop) |
| --- | --- | --- | --- |
| Tool surface | exactly ours | ours plus Claude Code's built-in shell and file tools, which must be switched off | ours as custom tools, answered by our process (it must reach Prometheus through the laptop's SSM session anyway) |
| Approval gate and checks | our code | our code, after Claude Code | our code |
| Same toolchain as the store | yes (Go, like `cmd/kvmcp`) | yes for the MCP server | REST/SDK, beta |
| Credential | an Anthropic API key (≈ $0.10–0.25 per run: ≈ 20–40 k input, ≈ 2–4 k output tokens at $4/$20 per MTok) | the author's Claude subscription | API key, plus session runtime; session history kept until deleted |

**Chosen:** the Go program, model `claude-opus-5-5`, adaptive thinking, structured output
for the proposal (the typed schema doubles as the first check), run in a container with
only the `status` key, the Anthropic key and a route to Prometheus and the coordinator.
Each run is one short loop (a handful of tool calls) started by the author
(`make ops-agent`) or by kvscale when an info alert fires. **Fallback** if the author has
no API key: the same tools as a read-only MCP server under Claude Code headless with
built-in tools disabled; the proposal schema, checks, container and gate don't change.

### Health and version endpoints

`/health` keeps its M4 body (scripts and the lab check use it). Added on the coordinator
and every replica, on the API port, without a key (like `/health`): `/health/live` (200
while the process runs), `/health/ready` (the readiness rule from the spec; the
coordinator's is today's `/health` status), `/version` (`buildinfo.Version`, a new
`buildinfo.BuildTime` set by the linker, `runtime.Version()`, role, shard/replica).
`kv_build_info` on the metrics port. The blackbox probe keeps its authenticated read; the
executor and `make smoke-aws` use `/version` to check every process runs the release.

### Monitor capacity

M4: Loki ≈ 420 MB after 35 minutes of benchmark (≈ 0.8 GB/h), 4.7 GB free of 8 GB. M5's
final runs push 2–3× the request rate, with three sets: ≈ 2–2.5 GB/h, so 8 GB lasts about
two hours.

| | Bigger volume only | Fewer log lines (sample successful requests) | **Bigger volume + Loki retention + a check** |
| --- | --- | --- | --- |
| Keeps M4's "follow one request through the logs" | yes | no (a sampled-out request has no lines) | **yes** |
| Cost | gp3 30 GB ≈ $0.003/h while up | none | **≈ $0.003/h** |

Chosen: the monitor's data directory on a **30 GB gp3** volume (root stays 8 GB), Loki
`retention_period: 12h` with the compactor (a session is ≤ 4 h, so nothing we need is
lost), and the lab check prints free space before and after; the dashboard gets
Prometheus' and Loki's own storage-size metrics. Supersedes nothing: ADR-0029's decision
stands; its risk row is Medium for M5 and mitigated here.

### Lab session plan

| Session | Work |
| --- | --- |
| 1 | Deploy; smoke incl. `/version`; leader placement drill; calibration (stops on coordinator saturation); one manual join and leave with the verifier and before/after sizes |
| 2 | Autoscaling on: scale-out and scale-in under a load step; agent proposals with one approved and executed; comparison runs (A and B on 2 and 3 sets) |
| 3 | Gradual and spike profiles, each from 2 sets, with a reset (leave + cooldown) between them |
| 4 | Sustained profile; export; final cost |

A warm standby shard-3 (booted and pending before the spike) is **not** planned; it would
show migration speed instead of boot speed, and the author can still ask for it (≈ $0.20 a
session).

### Cost

| Item | Estimate |
| --- | ---: |
| 4 lab sessions × ≈ 3 h × 7–9 instances (coordinator t3.medium, rest t3.small) | ≈ $2.2 |
| Cross-AZ: calibration, comparison runs, 3 profiles, migrations (≈ 80–120 GB) | ≈ $0.8–1.2 |
| Monitor data volume, public IPs, credits | ≈ $0.2 |
| Agent runs and evaluation set (if the API is used; billed to the author's Anthropic account, not the lab) | ≈ $3–8 |
| **M5 in the lab** | **≈ $3.2–3.6** |

Leaves ≈ $26 of credit; runway to about 2026-12-18 at the fixed ≈ $1.25 a day. The app
stack is deleted after every session; shard-3's groups go with it.

## Risks / Trade-offs

- [p99 doesn't respond to capacity below saturation] → calibration finds the knee first;
  the profiles drive a target rate with enough threads to queue (ADR-0032).
- [The coordinator saturates first] → t3.medium; leader-side confirmation; calibration
  stops if it happens (ADR-0032, ADR-0035).
- [The benchmark client becomes the bottleneck] → M2 reached 6,900 ops/s from one t3.small
  (workload B); the client's CPU is recorded; a target rate above what it can drive is
  flagged in the results.
- [t3 credits: new nodes start with few] → `unlimited` credits (ADR-0014); CPU credit
  surplus read from EC2 metrics and reported with the results.
- [Migration and transfer correctness] → fences at the data owner (freeze record,
  step-down), the committed-only tail; tests that inject writes into the windows between
  hold, fence and switch, kill a source leader with an uncommitted write, and check every
  acknowledged write; the local cluster gets a third replica set in Docker Compose for the
  same tests end to end; a verifier in the lab.
- [Instance cap below 9] → checked before the first session; a new ADR if so.
- [More holds in the request path] → `kv_write_hold_seconds` histogram; tests assert ≤ 2 s.
- [Agent cost or availability] → the agent is optional at run time; the autoscaler doesn't
  depend on it.

## Migration Plan

1. Spec gate (this change, ADRs accepted, AI review done).
2. Code and tests locally, in this order: health/version; record format 3; leader placement
   and transfer; pending sets and migration; scaling rules and executor (against the local
   stack, where "launch" starts shard-3's containers); agent.
3. Lab sessions 1–4 as planned above (the author deploys and deletes).

Rollback: every step is a release; `make up-app VERSION=<M4 release>` brings back M4's
behaviour on a fresh stack. M4 binaries refuse format 3 logs, so M5 starts from empty
volumes, as M3 and M4 did.

## Open Questions

- The lab's instance cap (read-only check before session 1); a cap below 9 means a new ADR
  for the scaling unit.
- The exact copy rate and its budget on the source's write p99: set from the calibration
  run.
