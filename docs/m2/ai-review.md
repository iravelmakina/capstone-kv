# M2 AI design review

> The critique must come from a **fresh AI session with no project context**, not from
> the session that drafted the design (course rule; CLAUDE.md rule 5). This file holds the
> prompt, the transcript and my verdicts.

**How it was run:** Claude Code (Opus 5.5) started a separate agent with no access to the
drafting conversation; it received only the prompt below, worked read-only in the
repository, and read the files the prompt lists plus the code it chose to check
(19 tool calls, ~6 minutes, 2026-11-24). Its final message is saved below verbatim.

**Timing note:** the review ran while `docs/m2/control-questions.md`, `docs/m2/cost.md` and
this file were being written, so its remark that the README links to files that don't exist
was true at that moment; they exist now.

## Prompt

```text
You are a senior cloud architect reviewing a student's design. Be critical: your job is
to find weaknesses, not to praise. Work READ-ONLY: do not edit, create, or delete any file,
and do not run commands that change anything (no git commits, no AWS or network calls).
You may read files in the repository.

Context: a course project to build a small DynamoDB-like key-value store over five
milestones (M1 network/IAM foundation, M2 sharding, M3 replication with quorum reads, M4
monitoring and an MCP server, M5 autoscaling on p99 and shard rebalancing). It runs in a
restricted AWS Academy sandbox: no IAM role creation, instances limited to t3.medium, no
ECS/ECR, no AWS CI/CD services, temporary credentials that expire every 3-hour session, and
the instance role can read S3 but not write it. This is milestone 2: a router
("coordinator") with a consistent hashing ring, shards, IaC, a CI/CD pipeline, and a YCSB
baseline.

Read these documents first (the course brief's M2 section is the requirement):
- docs/course/capstone-brief.md (sections "How this works", "Applies to every milestone",
  "Milestone 2", and skim M3-M5 for what M2 must prepare)
- openspec/changes/m2-sharding/proposal.md, design.md, tasks.md, and every file under
  openspec/changes/m2-sharding/specs/
- docs/adr/0009-runtime-and-delivery.md through docs/adr/0016-benchmark.md
- docs/m2/benchmark.md, docs/m2/pipeline.md, docs/m2/ring-distribution.md
- api/openapi.yaml
Then look at the implementation where you need to check a claim: internal/ (ring, store,
shard, coordinator), cmd/kvnode/main.go, infra/app.yaml, infra/artifacts.yaml,
scripts/node-boot.sh, Makefile, .github/workflows/ci-cd.yml and benchmark.yml, bench/.

Give:
1. The 5 most serious weaknesses, ranked: what is wrong, why it matters for later
   milestones (M3-M5), and what you would do instead.
2. Any decision where a rejected (or unconsidered) alternative is actually better.
3. Anything M2 should cover per the brief but doesn't, or covers only on paper.
4. Claims in the documents that the code or the evidence does not support.
Be specific: cite the document and section, or the file and function. Your final message is
the full review text; it will be saved verbatim as a transcript, so write it as the review
itself, in markdown, with no preamble about what you did.
```

## Transcript

# AI design review: capstone-kv, Milestone 2 (sharding)

Reviewer stance: critical, read-only. Sources: the brief, the `m2-sharding` OpenSpec change, ADR-0009 to ADR-0016, `docs/m2/*`, `api/openapi.yaml`, and the code under `internal/`, `cmd/`, `infra/`, `scripts/`, `bench/`, `Makefile` and `.github/workflows/`.

---

## 1. The five most serious weaknesses, ranked

### 1.1 Ring membership is soft state that anyone can change, and nothing persists it

**What is wrong** (`internal/coordinator/membership.go`, `coordinator.go`):
- **Membership is not persisted.** Readiness depends only on a count: `Ready()` returns `len(m.members) >= m.expected`. The coordinator does not record which shard IDs make up the ring.
  - Suppose a cluster has three shards and the coordinator restarts with `expected=2` (hard-coded in `infra/app.yaml`). It starts serving as soon as any two shards re-register.
  - The third shard's keys are then routed to the other two. Reads return **404 for data that exists**, and writes land on the wrong shard.
  - When the third shard re-registers, those writes become invisible. The result is silent divergence, not a 503.
- **`Remove` does not stick.** `Register` adds every unknown ID to the ring, and a live shard heartbeats every 5 s. So `DELETE /internal/v1/shards/{id}` lasts at most 5 s before the shard rejoins. `TestExplicitRemove` passes only because the test cluster runs no heartbeat loop.
- **The same ID can silently take over another node's slot.** `Register` with a known ID just overwrites `address`. Two causes lead to the same failure: a misconfigured second node with the same `KV_SHARD_ID`, or (in M3) two replicas of one shard. Either way the address flips back and forth on every heartbeat.
- **The admin endpoints sit on the public listener with no authentication.** `/internal/v1/shards` (register, list, delete) is served on the same port the security group opens to the whole VPC CIDR and to `AllowedClientCidr`.
  - Anyone in those ranges can post `{"id":"shard-1","address":"http://anything"}` and take shard-1's traffic, including the write bodies.
  - The coordinator will then send requests to any URL that starts with `http`, which is an SSRF primitive.
- **There is no ring epoch.** Shards accept any key from anyone and never check that they own it.

**Why it matters later:**
- **M3** needs replica sets: several nodes per shard ID, a leader, and followers that serve reads. The current model is one ID, one address and one ring identity, so it cannot express that. It would actively break, because replicas would overwrite each other's address.
- **M5** adds and removes shards at runtime and migrates data while "the old shard keeps serving reads". That requires an authoritative, versioned ring that survives a coordinator restart and that shards can check requests against. None of this exists yet.
- **M4** asks you to define what an agent may do through MCP. If the MCP server talks to the coordinator's port, ring administration is exposed to the agent along with the data operations.

**What I would do instead:**
- Persist the ring configuration (member IDs plus an epoch) in the store the coordinator already has for tables.
- Make joining and leaving explicit admin operations. Heartbeats should only update liveness for IDs that are already known; an unknown ID goes into a "pending" state rather than straight onto the ring.
- Define readiness as "every persisted member has been seen", not as a count.
- Move the admin endpoints to a separate port that is open only to the shard security group and an admin CIDR.
- Send the ring epoch with every coordinator-to-shard request, so M5 can fence a stale coordinator.

### 1.2 The storage engine is not a foundation for M3 and M4, and it can lose acknowledged writes

**What is wrong** (`internal/store/store.go`):
- **Writes are serialised and block reads.** `Put` and `Delete` hold the store-wide `sync.RWMutex` write lock while `fsync` runs. Writes to a shard therefore go one at a time, and every read waits behind each fsync, because Go's RWMutex blocks new readers once a writer is waiting. `benchmark.md` §"What the numbers say" 1 and 4 describe exactly this effect.
- **A failed write can later destroy good data.** If `appendRecord` fails partway (for example ENOSPC or EIO), the file offset is already past the partial bytes and nothing truncates back. The next successful `Put` appends after the garbage. On restart, `replay` stops at the bad record and `Open` calls `f.Truncate(good)`, which **deletes every later record, including acknowledged ones**.
- **Any damage, anywhere, silently drops the rest of the log.** `replay` treats every `io.ReadFull` error (a real EIO included), every CRC mismatch and every unknown op as "end of log". It then truncates. Damage in the middle of the log removes everything after it without a word. Only damage at the tail should be repaired; damage in the middle should stop the shard from starting.
- **Records carry no LSN, no version or timestamp, and no format byte.**
- **There is no compaction.** The data lives on the AMI's default root volume (no `BlockDeviceMappings` in `app.yaml`).
  - One benchmark session (load plus 3×A plus 3×B) appends about 1.75 M records of roughly 1.1 KB, about 2 GB across the two shards.
  - At the measured write rate (about 450 writes/s per shard, by your own estimate), the M5 "sustained high load" test fills the volume in a few hours.
  - A full disk then triggers the truncation bug above.

**Why it matters later:**
- **M3** says followers "track offset and upon restart resync only the relevant log range". Byte offsets are not stable here, because truncation reuses them. A follower that has consumed offset X may later receive a *different* record at X.
- **M3** quorum reads return "the newest version among them". **M4** requires LWW with versions or timestamps. Neither is possible without a version in the record and in the internal API.
- **M5** migration needs to enumerate keys by hash range. That is only possible today through `Keys()`, which takes the read lock and copies the whole key set.

**What I would do instead:**
- Version the record format now: `[format][LSN u64][timestamp/version][op][key][value]`.
- Use group commit: one fsync for a batch of waiting writes. Keep the log mutex separate from the map lock.
- On a write error, truncate back to the last good offset and switch the shard to read-only.
- Repair damage only at the tail; refuse to start on damage in the middle.
- Plan snapshot plus compaction (see 2.3).

### 1.3 The infrastructure cannot grow into M3 or M5, and every deploy is a full outage

**What is wrong** (`infra/app.yaml`, `scripts/node-boot.sh`):
- **Each node is a hand-written instance resource.** `Shard1` and `Shard2` are copies of the same block. M3 needs at least 6 shard nodes (2 shards × 3 replicas).
- **The AMI can change under you.** `ImageId` is an `AWS::SSM::Parameter::Value` that points at `al2023-ami-kernel-default` (the "latest" AMI), and `make up-app` never pins it.
  - CloudFormation resolves that parameter again on every stack update.
  - Once AWS publishes a new AL2023 AMI, the next routine deploy **replaces every instance**. That deletes the shard data on the root volume and the coordinator's table registry, and changes the shards' private IPs.
  - `pipeline.md` run 3 ("updated in place … no replacement") holds only because the AMI did not change that night.
- **Every release takes every node down at once.** A change to `AppVersion` changes the user data, which forces a stop and start of all three instances. Because `pipeline.md` says "every commit on `main` is a release", even a template-only commit restarts every node.
  - Nothing gates on health: there is no `CreationPolicy`, no `cfn-signal` and no rolling update.
  - CloudFormation reports `UPDATE_COMPLETE` even if the container never starts.
  - When the smoke test fails, nothing rolls back.
- **The expected shard count is hard-coded.** `expected-shards` is the literal `2` in the coordinator's user data.

**Why it matters later:** M5 asks you to scale on p99 and asks the control question "How do you ensure zero downtime during shard addition/removal?". The current model has a full outage on every commit and can lose data whenever the AMI changes. ADR-0014 accepts that M5 rewrites this, but M3 already roughly triples the node count.

**What I would do instead:**
- Use one launch template, parameterised by shard ID or replica-set ID.
- Pin `ImageId` to a concrete AMI ID and change it deliberately, in its own commit.
- Put data on a separate EBS volume (or accept rebuilding from replicas once M3 exists).
- Add `cfn-signal`, or better, one Auto Scaling group per shard (size 1 now, the replica set in M3) with `UpdatePolicy` rolling updates and `WaitOnResourceSignals`. That gives health-gated deploys with automatic rollback.
- Skip the app deploy when the image digest has not changed.

### 1.4 The pipeline can show green without deploying anything, and it auto-applies changes to the shared network

**What is wrong** (`Makefile` target `diff`, `ci-cd.yml`):
- **Every failed change set counts as success.** When `wait change-set-create-complete` fails, `diff` prints the reason, deletes the change set and runs **`exit 0`**. It treats *every* failure as "no changes".
  - A change set that fails to compute (a missing import, an unresolvable parameter, an invalid property value) therefore leaves `deploy-app` green.
  - `smoke-aws` then reads `AppVersion` **from the stack's outputs**, which is the *old* version, and checks that the old version runs. The smoke test passes.
  - Net result: a green pipeline that deployed nothing.
- **The network stack is auto-applied from any push to `main`.** `deploy-infra` runs `make up` on it with `CONFIRM=yes` and no check of the `Replacement` column. A change that replaces the VPC or a subnet would be executed without review. ADR-0015 lists this only as "network changes are rare".
- **Rollback covers the image only.** A `workflow_dispatch` with `version=` swaps the image and `node-boot.sh`, but it deploys the *current* `app.yaml`, network and artifacts templates. If the user data or the boot-script arguments changed between the two versions, the rollback breaks.
- **Old releases expire.** `expire-old-releases` deletes `releases/` after 60 days. That is likely before M5, when you will want to redeploy the M2 release for a like-for-like comparison.
- **The benchmark workflow has never run green** (see §4).

**What I would do instead:**
- Treat as success only the "didn't contain changes" `StatusReason`; fail on anything else.
- Have the smoke test assert the SHA the job *intended* to deploy (`github.sha` or the input), not the stack output.
- Fail `deploy-infra` on `Replacement=True` for the network stack.
- Tag the baseline release so the lifecycle rule skips it.

### 1.5 The benchmark is a single saturation point, not a baseline M3 and M5 can compare against

**What is wrong:**
- **It measures one point at saturation.** go-ycsb runs a closed loop: 16 threads, no `target` rate (`bench/workloads/common`). Every number is taken at maximum throughput, so the p99 measures queue depth at saturation. Change the thread count and the p99 changes.
- **Nothing is measured on the server side.** There is no per-shard request count and no per-shard key count, although shard `/health` already exposes `keys`. There is no split of latency into coordinator, shard and fsync, and no EBS metrics.
- **The data and log are not reset between runs.** A runs three times and B three times on the same data and an ever-growing log.
- **Only summaries are kept** (see §4).

**Why it matters later:**
- M3 asks "how did p99 change after replication, and why exactly". If both milestones measure only at saturation, the answer will mix extra latency with lost throughput.
- M5 scales on a p99 threshold, so it needs latency as a function of offered load, to know where the threshold sits.
- With zipfian keys, the hot key stays on one shard. Adding shards may not move p99 at all, and without per-shard data you cannot show that.

**What I would do instead:**
- Keep the current saturation run, and add runs at fixed `target=` rates (for example 25/50/75/90% of the measured maximum) to get a latency-versus-throughput curve.
- Record per-shard key and request counts before and after each run.
- Export server-side histograms (this is also the start of M4).
- Store the full per-run output.

---

## 2. Decisions where the rejected or unconsidered alternative is better

1. **Fixed logical partitions (slots) instead of a pure virtual-node ring** (not considered in ADR-0011).
   - Hash into, for example, 1024 fixed slots, and map slots to shards with an explicit, persisted table (Redis Cluster style; DynamoDB partitions are similar in spirit).
   - Balance becomes a decision, not a statistical outcome.
   - M5 migration becomes "move slot k from A to B, old owner serves reads until done": a unit you can track, fence with an epoch and show on a dashboard.
   - The ring version comes for free, which fixes 1.1.
   - The brief allows "consistent hashing **or similar**".
   - The vnode acceptance test also proves less than it claims (§4, item 11).
2. **ADR-0014 option 2 (Auto Scaling groups), rejected for M2.**
   - One ASG of size 1 per shard keeps stable IDs: the ID comes from the ASG or launch-template parameter, so the "needs a scheme" objection goes away.
   - It adds self-healing and health-gated rolling updates with automatic rollback.
   - It is also the structure M5 needs.
   - The cost is the same instances. Check in the lab that ASGs are allowed.
3. **ADR-0013 storage option 3 (embedded engine), rejected as "a library's internals".**
   - M3 to M5 will need compaction, versions, scans by hash range and snapshots for follower bootstrap. You will end up writing a small LSM yourself.
   - A defensible split: Pebble or bbolt holds the *state*, and your own append-only log, with LSNs, is the *replication* log for M3. That keeps the part that is graded (the log) and drops the part that is not (compaction).
4. **ADR-0013 membership: a hybrid of options A and B, which was not considered.**
   - Membership changes are explicit and persisted (option A's safety). Heartbeats only report liveness (option B's dynamism).
   - This still meets "registered upon start": registration goes into a pending state, and an admin action admits the shard.
5. **Release identity: an image digest, not "every commit is a release"** (`pipeline.md` Notes). A docs-only or template-only commit should not restart the data nodes.
6. **Coordinator body handling.** `readJSONObject` unmarshals and recompacts every PUT body on the node that ran out of CPU credits (`benchmark.md`, CPU table).
   - Checking only that the body starts with `{` plus a size limit, or validating with a streaming parser without building a map, is cheaper.
   - It also leaves the coordinator able to pass bodies through untouched in M3, when it has to fan out.

---

## 3. What M2 should cover per the brief but doesn't, or covers only on paper

- **"Shards are registered in the coordinator upon start (API needs to be added)."** The registration API exists in code, but `api/openapi.yaml` explicitly leaves `/internal/v1/...` out of the contract, and ADR-0010 calls it "not part of the public contract". The brief names this API as a deliverable. It has no spec, no schema and no contract test.
- **"Use consistent hashing … for dynamic updates."** A dynamic update today (adding a shard while data exists) remaps about a third of the keys to a shard without their data. Those keys answer **404, not 503**: wrong answers that look valid. ADR-0013 avoids the problem by policy ("adds shards only before loading data"). Dynamic updates are therefore unsafe, not just unmigrated. At minimum, a remapped key on a ring with migration pending should answer 503.
- **The benchmark is kept "in its own CI job"**, but the only recorded run of that job failed, and the baseline came from a laptop (§4).
- **README and the defense artefacts.** Tasks 9.1 to 9.5 are open. `README.md` already links to `docs/m2/cost.md`, `control-questions.md` and `ai-review.md`, none of which exist. The brief requires the README to cover "what changed since the previous milestone".
- **Control question "Is your coordinator a bottleneck? How would you scale it?"** The prepared answer ("stateless apart from the table registry, scale out behind a load balancer") is wrong for this code (§4, item 5).
- **Control question on linearization and serialization.** Nothing in the design discusses it. Per key, M2 is close to linearizable (one owner, a mutex). But the coordinator's 3 s timeout turns a committed PUT into a 503, an ambiguous outcome. That is worth stating before M3 makes it worse.
- **Preparing for M3 and M4, missing from the ADRs:**
  - Shards in private subnets have no SSM or CloudWatch interface endpoints and no route out. You cannot open a session on a shard, read its container logs, or push metrics or logs from it. M4 needs a decision: the coordinator scrapes shards over 8080, or you pay for interface endpoints.
  - The delivery path (ADR-0009) handles only your own image. Any third-party image for M3 or M4 (RabbitMQ or Kafka, Prometheus, Grafana) has no registry to come from. It must also travel through S3 as a tarball.
  - M3's "physical file on S3" log option is blocked, because the instance role cannot write to S3. Record this in M2's constraints, because it pushes M3 towards the shard-local log, which makes 1.2 urgent.

---

## 4. Claims that the code or the evidence does not support

1. **ADR-0014, "Parameters: … shard count expected by the coordinator".** It is not a parameter: `app.yaml` passes the literal `coordinator 2` to `node-boot.sh`.
2. **Spec `app-deployment`: "reaches `CREATE_COMPLETE` and every node answers `GET /health` with 200".** No `CreationPolicy` or `cfn-signal` exists, so `CREATE_COMPLETE` says nothing about health. `aws-deploy.txt` D4 shows the coordinator became ready 49 s *after* `CREATE_COMPLETE`.
3. **Spec `kv-api`: "Every node SHALL answer `GET /health` with 200 while it can serve requests".** The coordinator returns 200 with `"ready":false` while it answers every item request with 503 (`coordinator.go` `health`). The shard's `/health` returns 200 regardless of the state of its log or disk. Any M5 load-balancer or ASG health check built on this will route traffic to nodes that cannot serve it.
4. **Spec `kv-api`: "every public endpoint's responses validate against `api/openapi.yaml`".** `contract_test.go` checks only that each status code is documented and that error bodies have the form `{"error":...}`. It validates no response body against `Item`, `TableDefinition` or `Health`.
5. **`design.md` Risks and `benchmark.md` point 3: "stateless apart from the table registry, so it can be scaled out behind a load balancer".** Two things make the coordinator stateful:
   - Membership and liveness live in each process's memory, and each shard heartbeats to a single coordinator IP baked into its user data (`KV_COORDINATOR_URL`). Behind a load balancer, every coordinator would see shards flapping between up and down.
   - The table registry is a file on one instance's root disk.
6. **ADR-0013, "Groundwork for M3 log: the same log format".** The format has no LSN, no version and no format byte, and the truncation in `Open` reuses offsets (1.2).
7. **ADR-0013 and the `membership.go` comment "takes a shard off the ring for good".** The shard's next heartbeat puts it back (1.1). The test that "proves" removal runs without heartbeats.
8. **`benchmark.md`, "Writes are limited by the disk, not the CPU" and "~450 per shard per second".**
   - Nothing measured how requests split between shards; zipfian keys make an even split unlikely.
   - Nothing measured EBS (queue length, write operations).
   - About 450 fsync/s is far below the IOPS ceiling of the default volume. The limit is the store's design (a global lock across fsync), not the disk. The distinction matters, because the cure is group commit, not "more shards".
9. **ADR-0016 and spec `benchmark-baseline`: "raw output SHALL be kept in S3".** Only the final summaries are kept (`bench.sh` prints from the "finished" line onwards). The per-run raw output stays in `/opt/kvbench/out` on the client, which is deleted with the stack. `benchmark.md` honestly says "raw summaries", but the spec still claims raw output.
10. **Spec `benchmark-baseline`: "WHEN the benchmark workflow runs THEN each workload completes 3 runs".** The only recorded workflow run (`pipeline.md` run 6) failed. The baseline came from `make bench-aws` on a laptop. The fixed workflow has no recorded successful run.
11. **ADR-0011 and `ring-distribution.md`: "128 virtual nodes … confirmed".** The test measures one placement: the exact IDs `shard-1` to `shard-5`.
    - With V virtual nodes, a shard's share of the ring has a relative spread of about 1/√V, roughly 9% at 128. The 8.7% worst case is one draw from that distribution, not a property of 128.
    - Your own table shows how noisy single placements are: 64 vnodes give 0.5% with 2 shards and 27% with 3; 256 vnodes are worse than 128 with 2 shards.
    - Different IDs in M5 (anything ASG-generated) carry no guarantee. Repeat the test over many random ID sets and report a percentile.
    - Also, "keys shaped like the benchmark's (`user0`…`user999999`)" is inaccurate: the benchmark uses `insertorder=hashed`, so its keys are `user` followed by a hashed number.
12. **`pipeline.md` run 5: "A rollback is one parameter".** It rolls back the image only, and only within the 60-day lifecycle rule (1.4).
13. **`pipeline.md` run 3: "updated in place … the table … survived".** True for that run. The template makes a replacement, and the data loss that comes with it, likely on any deploy after a new AL2023 AMI is published (1.3).
14. **ADR-0015: "every stack update goes through a change set".** True, but a failed change set is reported as success (1.4), so the guarantee is weaker than it reads.
15. **ADR-0009 Risks: "Go garbage collection … measured in the benchmark (p99)".** No GC data was collected (no `gctrace`, no runtime metrics). `benchmark.md` lists GC only as a *possible* cause of the 250 ms stalls.
16. **Minor bookkeeping.**
    - ADR-0010 ("Implemented in: `internal/api/`") and the code layout in `design.md` name `internal/api/`, which does not exist; the handlers are in `internal/coordinator/`.
    - `tasks.md` leaves 7.2 (the ci-cd workflow) unchecked while 7.3, which depends on it, is checked.

## My verdicts

*Proposed by the drafting session and agreed by the author in chat on 2026-11-24. The changes are tasks 10.1–10.8 in `openspec/changes/m2-sharding/tasks.md`; the two that change a decision have their own ADRs (0017 AMI, 0018 log damage).*

The three argued at the defense:

| # | AI point | Verdict | Argument | Change |
| --- | --- | --- | --- | --- |
| 1 | §1.4: a failed change set counts as success, and the smoke test checks the version the *stack* reports, so a green run can deploy nothing | **Accept** | Confirmed in the Makefile: `diff` runs `exit 0` on any change-set failure. "No changes" is the only failure that is fine; anything else must fail the job. The smoke test must compare against the version the job *meant* to deploy, not the one the stack happens to report | `diff` succeeds only on "didn't contain changes"; `smoke-aws` takes the intended version from CI |
| 2 | §1.3: the AMI parameter re-resolves "latest AL2023" on every update, so a new AMI replaces every instance and wipes shard data | **Accept** | The SSM-typed parameter is looked up again at each update; nothing pins it. Losing all data on a routine deploy because AWS published an AMI is unacceptable, and cheap to prevent | `ImageId` becomes a concrete AMI ID: looked up once when the stack is created, reused on every update, changed only deliberately |
| 3 | §2.3: use an embedded engine (Pebble, bbolt) instead of our own log | **Refute** | The course grades building the store: M3's replication log is the point, and an engine whose internals I can't explain fails "without notes". Its valid concerns are taken without the engine: the two data-loss bugs in §1.2 are fixed now; versions/LSNs in the record and group commit come with M3, where replication needs them | Store: a failed write truncates back to the last good record; damage in the middle of the log stops the shard instead of being silently cut |

Other points:

| Point | Verdict | What happens |
| --- | --- | --- |
| §4.3 `/health` is 200 while the coordinator can't serve | Accept | `/health` answers 503 until ready (body unchanged) |
| §4.1 "expected shard count" is not a parameter | Accept | `ExpectedShards` parameter in `app.yaml` |
| §4.5 coordinator is not "stateless apart from the table registry" | Accept | Wording fixed in `benchmark.md`, `design.md`, control question 7: membership lives in its memory and shards heartbeat to one IP |
| §4.8 "limited by the disk" | Accept, sharper | It's the single lock held across `fsync` (latency-bound, far below the volume's IOPS), so the cure is group commit before more shards; wording fixed |
| §4.11 128 vnodes "confirmed" by one placement | Partly accept | Valid for the fixed IDs we deploy (`shard-1…5`); M5 with other IDs needs a percentile over many ID sets. Key description in `ring-distribution.md` corrected |
| §1.1 `Remove` doesn't stick; membership not persisted; admin endpoints on the public port | Accept, deferred | A persisted, versioned ring with explicit join/leave and a separate admin port is M5's rebalancing design (ADR then). In M2 the port is reachable only from the VPC and my IP. The comment "for good" is corrected now |
| §3 registration API has no spec | Accept | Documented in `api/internal-openapi.yaml`, separate from the public contract (ADR-0010 stands) |
| §1.2 no LSN/version, no compaction; §2.1 slots; §2.2 ASGs; §1.5 latency curve, per-shard counts | Accept, deferred | M3 (versions, LSN, group commit), M5 (slots or ring with epoch, ASGs, migration); benchmark additions in M3 so both milestones measure the same way |
| §4.9 / §4.10 raw output and the workflow never green | Accept | Only summaries leave the client (instance role can't write S3); stated in `benchmark.md`. The fixed workflow gets a green run after these fixes |
| §4.16 bookkeeping | Accept | Task 7.2 ticked; `design.md` no longer names `internal/api/` (ADR-0010 is accepted and stays; noted here) |
