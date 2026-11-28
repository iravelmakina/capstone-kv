# Tasks

> Group 1 is the spec gate: no code or IaC until 1.5 is done. AWS writes (deploys, deletes,
> capacity changes) are run by the author; Claude Code runs local tests and read-only checks.

## 1. Spec gate

- [x] 1.1 AI design review of this change and ADRs 0032–0036 from a fresh session with no project context; transcript and verdicts in `docs/m5/ai-review.md`; ADRs, design and specs revised per the verdicts the author confirmed
- [x] 1.2 Author confirms the agent runtime (API key or the Claude Code fallback), reviews proposal, design, specs and ADRs 0032–0036, and sets them to `Accepted` in chat; ADR-0013, ADR-0022 and ADR-0031 get "Superseded in part by" lines (texts unchanged)
- [ ] 1.3 Read-only check of the lab's instance limit (lab page, read by the author) recorded in `docs/m5/aws-limits.md`; if below 9, stop and write a new ADR for the scaling unit
- [x] 1.4 `openspec validate m5-production --strict` passes
- [x] 1.5 Spec-only commit (no code, no templates); push `m5-production`

## 2. Health and version endpoints

- [x] 2.1 `/health/live`, `/health/ready`, `/version` on coordinator and replicas; `buildinfo.BuildTime` set by the linker in the Dockerfile; `kv_build_info{version,role}`; `/health` unchanged — verified by handler tests (live while not ready, replaying → 503 "replaying", version from the binary) and `make smoke` locally
- [x] 2.2 OpenAPI contracts updated and the contract test passes; `make smoke-aws` checks `/version` on every process (used from session 1)

## 3. Record format 3

- [x] 3.1 `internal/store`: freeze/unfreeze and purge records (≤ 1,000 keys each) in format 3; format 2 still read; a binary given an unknown format refuses the log — store tests: replay with purge removes keys without tombstones, frozen key sets rebuilt on replay, a complete record in a newer format refused even as the last record (an M4 binary, which predates this check, refuses format 3 only when more records follow it; M5 starts from empty volumes)

## 4. Leader placement and transfer

- [x] 4.1 Bounded first-assignment wait (all expected replicas or 3 minutes with a majority) and the tie-break toward leaderless nodes; `kv_node_leaders`; `LeadersColocated` rule — unit tests: a late replica within 3 minutes gets its preferred leadership; equal `(epoch, LSN)` picks the other node; a more up-to-date follower on a busy node still wins; `promtool` test for the alert
- [x] 4.2 Step-down on the leader, synchronous epoch push to every replica, planned transfer (`POST /v1/admin/shards/{id}/leader`) with the ≤ 2 s write hold and abandon path — tests: a write injected while the leader steps down is either on the new leader or refused and retried, never lost; a lagging target abandons without raising the epoch; a writer loops through 20 transfers with every acknowledged write read back and no 5xx

## 5. Pending sets and rebalancing

- [x] 5.1 Set states (`pending`, `active`, `leaving`, `retired`) persisted in the coordinator's log; a registering unknown set stays pending; base sets can't be removed — tests: registration with data present remaps no key; removing shard-1 → 409
- [x] 5.2 Ring pair and routing by migration phase; migration and scaling state persisted and resumed; admin API (`/v1/admin/cluster`, `/migrations`, abort) with `admin` scope and a new `status` scope for cluster reads — tests: restart mid-copy resumes; second migration → 409; `status` key gets 403 on items and admin actions
- [x] 5.3 Replica side: commit point, state of moving keys as of it, committed-only filtered log pull, batch apply with given stamps (newer wins, ties by shard ID, HLC advanced), freeze and 409 "moved" — shard tests, including a source leader killed with an uncommitted write that never reaches the target
- [x] 5.4 Copy, tail, per-source fenced cutover with the hold and the 409 retry, purge, removal, abort; copy rate limit; migration metrics and `MigrationStalled` — tests: 100,000 keys, add shard-3 then remove it while a verifier writes; a write injected between hold and freeze and one during the freeze's commit wait; only moving keys copied; every acknowledged write reads back; key counts sum unchanged; no hold over 2 s
- [x] 5.5 Local Docker Compose with an optional third replica set; `make local-join` / `make local-leave`; before/after key counts from the local dashboard in `docs/m5/rebalancing.md` (local section)

## 6. Autoscaling

- [x] 6.1 12 ms bucket edge (ADR-0032) — the metrics test checks the edge list and that both SLO edges remain
- [x] 6.2 Prometheus rules `ScaleOutDue` (writes with leader confirmation, reads with replica confirmation, ≥ 100 req/s) and `ScaleInLatencyOk`, plus replica sets, live replicas and nodes on the dashboard — `promtool test rules` cases: write breach under load fires; coordinator-only breach doesn't; idle breach doesn't; flap pattern doesn't produce a scale-in
- [ ] 6.3 `infra/app.yaml`: launch template and three single-subnet Auto Scaling groups `velmakina-kv-shard-3a/3b/3c` (min 0, max 1, no `DesiredCapacity`, `AZRebalance` and `ReplaceUnhealthy` suspended, scale-in protection, replica ID tag), coordinator t3.medium, Prometheus `http_sd` from the coordinator, monitor 30 GB data volume and Loki retention 12 h; `make up-app` refuses while shard-3 isn't retired — `cfn-lint`, `make diff` shown to the author, local Prometheus finds replicas through `/sd`
- [ ] 6.4 `cmd/kvscale`: reads rules and guards from Prometheus, rate guard and cooldown from the coordinator's scaling state, the 5-minute post-hold guard, the leaving-set exception, scale-out and scale-in sequences, decisions logged and posted to Alertmanager, admin key read from Secrets Manager into memory — tests against fakes (Prometheus, coordinator, an ASG interface): every guard blocks, bounds hold, restart resumes; local run where "capacity" starts and stops the third Compose set
- [ ] 6.5 `make autoscale` (run by the author), README section "Autoscaling" (how to run it, what it needs, what happens with the laptop off)

## 7. Ops agent

- [ ] 7.1 `cmd/kvops` tools: firing alerts, the fixed named queries, cluster state (`status` key), executor decisions, release; inputs passed as structured data; runs in a container without `~/.aws` or the admin key — test: the tool list has no item, log or free-form query tool; the container has no AWS credentials and can't write approvals
- [ ] 7.2 Proposal schema (kinds, parameters, reasoning, evidence, risks), checks (kind, bounds, names exist, abort only before cutover, scaling only when the autoscaler is off or blocked), records of each run, delivery through Alertmanager — deterministic unit tests that don't call the model
- [ ] 7.3 Evaluation set: recorded situations with the expected kind (latency with the autoscaler on → `no_action`; autoscaler off → `scale_up`; node down → `no_action`; colocated leaders → `transfer_leader`; stalled migration → `abort_migration`; an injected annotation stays within bounds); `make ops-eval` reports the pass rate (run on request, billed)
- [ ] 7.4 Approval gate `kvops approve <id>` (outside the container; shows the proposal, asks, records approver, 10-minute expiry) executed by kvscale with guards re-checked — tests: expired and state-changed proposals refused
- [ ] 7.5 Demo transcript for the defense in `docs/m5/ops-agent.md`: proposals from a local run, one approved and executed, one refused by a guard, and the evaluation pass rate

## 8. Lab sessions (the author deploys and deletes)

- [ ] 8.1 Session 1: `make release`, `make up-app MONITORING=true BENCH=true`; smoke incl. `/version`; leader placement after a late node (or a node stopped and started), with a transfer through an approved proposal; calibration: target-rate steps on 2 sets with 64–128 threads, coordinator and leader p99 and coordinator CPU against rate into `docs/m5/benchmark.md`; stop for a new ADR if the coordinator saturates first or the thresholds don't sit between normal load and saturation
- [ ] 8.2 Session 1: one manual join and leave of shard-3 under load with the verifier; before/after keys, log bytes and request rate per shard from the dashboard into `docs/m5/rebalancing.md`; 5xx count, longest hold, verifier result
- [ ] 8.3 Session 2: autoscaling on; a load step triggers scale-out, new nodes take traffic, load drop triggers scale-in; the decisions log and Telegram messages saved; agent proposals with one approved and executed; comparison runs (unthrottled A and B, 16 threads, on 2 and 3 sets)
- [ ] 8.4 Session 3: gradual and spike profiles (`scripts/m5-bench.sh`), each from 2 sets with a reset between; session 4: sustained profile; latency, throughput, replica sets, live replicas and nodes over time exported before teardown; monitor disk before and after; comparison with the M2 baseline in `docs/m5/benchmark.md`
- [ ] 8.5 Each session: evidence in `docs/m5/evidence/aws-*.txt` with account IDs as `<ACCOUNT_ID>`; `make down-app` by the author; `docs/m5/cost.md` with actual spend

## 9. Defense material

- [ ] 9.1 README: how to bring M5 up, how to verify it, what changed since M4, that only shard-3 can be removed
- [ ] 9.2 `docs/m5/control-questions.md`: notes for the eight control questions, with the measured numbers, and why scale-in also checks the load (beyond the brief's rule)
- [ ] 9.3 `AGENT_WORKLOG.md` rows for every agent interaction of M5

## Workflow follow-up

- Archive `m4-monitoring` after its defense; archive this change after M5's defense.
