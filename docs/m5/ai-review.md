# M5 AI design review

> The critique must come from a **fresh AI session with no project context**, not from
> the session that drafted the design (course rule; CLAUDE.md rule 5). This file holds the
> prompt, the transcript and the verdicts.

**How it was run:** Claude Code (Opus 5.5), in the session that drafted M5, proposed a
separate session with no access to the drafting conversation, in read-only (plan) mode;
the author started it on 2026-11-27 ("Start the review"). It received only the prompt
below and read the drafts from the working tree of branch `m5-production` (status
Proposed, uncommitted). Its final message (2026-11-28) is saved below verbatim, with its
headings moved two levels down.

## Prompt

```text
You are a senior cloud architect reviewing a student's design. Be critical: your job is to find weaknesses, not to praise.

Context: a course project to build a small DynamoDB-like key-value store over five milestones (sharding, replication with quorum reads, monitoring and failure handling, autoscaling on p99). This is milestone 5: autoscaling on p99 latency, shard addition with background key migration, an ops agent that proposes actions behind a human approval gate, health/version endpoints, and a final YCSB benchmark. The store has 2 shards, each a replica set of 3 with a single leader (W = 2, R = 2), one coordinator, Prometheus/Grafana/Alertmanager/Loki on a monitor instance. It runs in a restricted AWS Academy sandbox (no IAM role creation, CloudWatch custom metrics denied, instances can't change Auto Scaling capacity, t3.small/medium only, instance cap unknown, credentials expire each ~3 h lab session, about $29.5 of a $60 credit left).

Read only these files in the repository at its current working tree (branch `m5-production`; the M5 files are uncommitted drafts). Do not change any file and make no AWS or network calls:
- the "Milestone 5" section of `docs/course/capstone-brief.md`
- `openspec/changes/m5-production/proposal.md`, `design.md`, `tasks.md` and `specs/*/spec.md`
- `docs/adr/0032-scaling-metric-and-thresholds.md`, `0033-rebalancing-algorithm.md`, `0034-ops-agent-autonomy.md`, `0035-scaling-unit-and-execution.md`, `0036-leader-placement.md`
- for background on what exists: `docs/adr/0011-hash-and-virtual-nodes.md`, `0020-replication-model-and-placement.md`, `0023-replica-sync.md`, `0028-record-versions-and-lww.md`, `0030-availability-slo-after-drills.md`, `0031-latency-histogram-buckets.md`, and `docs/m2/benchmark.md`

Give:
1. The 5 most serious weaknesses, ranked: what is wrong, why it matters (correctness of migration and leader transfer, whether autoscaling on p99 actually works, safety of the ops agent, cost and the sandbox), and what you would do instead.
2. Any decision where a rejected alternative is actually better.
3. Anything missing that milestone 5 should cover per the brief.
Be specific and cite the document and section.

Your final message must be the full review as Markdown, starting with a heading "Independent AI design review — M5 (m5-production)" and a line stating that you are a fresh session with no prior project context, the model you are, the date, and exactly which files you read. That final message is the deliverable: the author will paste it verbatim into `docs/m5/ai-review.md`, so do not write it to a file yourself.
```

## Transcript

### Independent AI design review — M5 (m5-production)

**Reviewer:** a fresh Claude Code session with no prior project context and no memory of how these documents were drafted. I deliberately did not read the shared session-notes folder, to stay independent. **Model:** Claude Opus 5.5 (`claude-opus-5-5`). **Date:** 2026-11-28. **Files read (working tree on branch `m5-production`, read-only):** `docs/course/capstone-brief.md` (the "How this works", "Applies to every milestone" and "Milestone 5" sections); `openspec/changes/m5-production/proposal.md`, `design.md`, `tasks.md`, `specs/{autoscaling,health-endpoints,leader-placement,load-testing,ops-agent,shard-rebalancing}/spec.md`; `docs/adr/0032`–`0036`; and for background `docs/adr/0011`, `0020`, `0023`, `0028`, `0030`, `0031`, and `docs/m2/benchmark.md`. Nothing else was opened, no files were changed, and no AWS or network calls were made.

---

#### 1. The five most serious weaknesses (ranked)

##### 1. A hold at the coordinator does not fence anything, so cutover and leader transfer can lose writes that were already acknowledged

**Where:** ADR-0033 Decision step 5; ADR-0036 Decision 2; spec `shard-rebalancing` "Cutover"; spec `leader-placement` "Planned leader transfer".

**What's wrong.** Both mechanisms hold *new* writes at the coordinator, read the source's or old leader's "last record", wait for the target to reach it, then switch. Nothing stops writes that were **already in flight** to the source or old leader when the hold started. A commit can take up to 2 s (ADR-0023, commit wait), so such writes are common under load.

- **Leader transfer, concrete failure.**
  1. The coordinator holds writes to shard-1 and sees that target T has acked old leader L's last LSN X.
  2. A request sent before the hold is appended on L at X+1. The third replica F still pulls from L, because neither replica knows the new epoch yet (heartbeats arrive every 5 s). F acks, L commits, and the client gets **200**.
  3. Meanwhile the coordinator has raised the epoch with T as leader. If T never pulled X+1, then when F learns the new epoch its last record isn't in T's log, and ADR-0023 step 5 truncates it.

  The result is an **acknowledged write lost**. This is exactly the "High" risk in ADR-0036's risk table, and its mitigation ("with writes held") doesn't cover the case.
- **Migration cutover:** the same race. An in-flight write to a moving key commits on the source after the coordinator read "last committed LSN". Either it never reaches the target (lost, then purged), or it reaches the target through a still-running tail *after* writes released to the target. In that case its order against those writes is decided by two leaders' HLC stamps, which is exactly the clock-skew hazard ADR-0033 cites to reject option 2.

**Why it matters.** "No committed write is lost" is the core promise of every M5 data-movement feature. The automatic leader rebalance (ADR-0036 Decision 3, on by default) would trigger this path without anyone watching.

**What to do instead.** Fence at the data owner, not at the router.
- **Transfer:** the coordinator first tells L to stop accepting writes for epoch e. L finishes or fails its in-flight commits and reports its final LSN. The coordinator waits for T to reach that LSN, persists e+1, **pushes e+1 synchronously to T, L and F**, and only then releases. This is Raft's leadership transfer (TimeoutNow) and Kafka's controlled shutdown.
- **Cutover:** the source leader writes a replicated "moving keys frozen" record at LSN F and rejects writes to those keys after it. The target must reach F, then the coordinator switches. This is the role of Redis Cluster's MIGRATING/IMPORTING states.
- **Minimum fallback** if you keep the coordinator-only design: count in-flight writes per source/shard and wait for that count to reach zero *after* starting the hold and *before* reading the LSN. Budget the 2 s accordingly (in-flight commits alone can use it all).
- Add a test that **injects an in-flight write between the hold and the LSN read**. The planned "writer loops through 20 transfers" test (tasks 3.2, 4.4) is unlikely to hit the window by chance.

##### 2. The migration tail ships records that aren't committed yet, which brings back writes the system had truncated

**Where:** design "Replica changes" (`GET /internal/v1/log?...&moving_to=` is "the existing pull with a key filter"); ADR-0033 Decision step 4; spec "Copy keeps up with writes".

**What's wrong.** Under ADR-0023, the follower pull returns records the leader has **appended but not yet committed**: the follower's pull *is* the ack that commits them (ADR-0023 Decision 2–3). The migration tail reuses that endpoint, and the target applies each record "through its normal write path … replicated with W = 2", so it **commits records on shard-3 that the source never committed**.

If the source leader then dies before a follower acks, promotion picks a follower without the record, and ADR-0023 step 5 truncates it on the source. The target still has it, and after cutover it is served. That breaks ADR-0020's explicit guarantee ("so a write that never committed can't come back", Decision 4). The "source leader changes during copy" mitigation in ADR-0033 makes it worse: the restarted copy's older value loses to the phantom's higher stamp, so the target keeps the phantom.

**Related gaps:**
- "State at an LSN" is paged (design: `after=&limit=`), so pages are read at different times. The spec should require that L is the leader's **commit index taken before the first page** and that the tail starts from that L. Today "as of an LSN it returns" is ambiguous per page.
- A new `PURGE` op 3 is added while "Format version stays 2" (design "Replica changes"). ADR-0028 kept a format byte exactly so a reader can refuse records it doesn't understand. An M4 binary meeting op 3 in a "format 2" log has undefined behaviour, and the rollback plan relies on "fresh volumes" instead of a safe failure. Bump the format to 3.
- A single purge record per source for ≈ 17k keys has no stated size or batching. Say how it is split, and confirm replay cost after a restart.

**What to do instead.** Serve the filtered tail only up to the source leader's **commit index** (or have the target apply a record only once the source reports it committed). Spell out L as the commit index. Bump the record format.

##### 3. Autoscaling on p99 measured at the coordinator is unlikely to close the loop: scaling adds capacity where the bottleneck probably isn't, and at the worst moment

**Where:** ADR-0032 (Context and Decision); design "Risks / Trade-offs"; spec `autoscaling` "Scale-out condition"; spec `shard-rebalancing` "Copy rate limit"; `docs/m2/benchmark.md` "What the numbers say" 1–3.

**What's wrong:**

- **The signal includes the component that doesn't scale.** `docs/m2/benchmark.md` point 3 says the coordinator is "the next bottleneck": it drained its CPU credits at ≈ 1,800 ops/s on a t3.small and handles every request twice. M5's load profiles must reach "at least twice" the knee (spec `load-testing`), with more routing work (two rings, holds). If p99 rises because the coordinator is queueing, adding shard-3 doesn't lower it. The executor then reaches the upper bound with the condition still firing, and scale-in is blocked until load drops. The design's risk list names the benchmark client and t3 credits on new nodes, but **not coordinator saturation**.
- **Migration starts when the sources are already saturated.** The scan, the filtered log read from disk and the tail all land on source leaders whose write path is one lock held across `fsync` (M2 point 1). The spec requirement "Copy rate limit … p99 at the coordinator stays below the scale-out thresholds during the copy" can't be met: by definition a scale-out copy starts while p99 is *above* the threshold. As written, the requirement can't be tested.
- **Reaction time against the spike profile:**

  | Step | Time |
  | --- | --- |
  | 2-min rate window + `for: 3m` | ≈ 4–5 min |
  | Boot | ≈ 3 min |
  | Register and assign leader | not stated |
  | Throttled copy + two cutovers | not stated |
  | **Total** | **≈ 9–12 min** |

  The spike lasts 15 min (spec `load-testing`), so capacity arrives near its end, and the spike profile will mainly show that scaling is too slow. ADR-0032 admits this as an accepted downside but doesn't design for it.
- **The read trigger is mostly noise.** The 12 ms read threshold sits about 1 ms above conditions scaling can't fix: a follower node stopped gives 11.4 ms (ADR-0032 table); M3's spread between deployments goes up to 10.3 ms. And reads are slowed mainly by waiting on the write lock (M2 point 4), so a genuine read breach usually comes with a write breach anyway.
- **The rate guard is too low for a meaningful p99.** At 10 req/s per shard, a 2-min p99 rests on its 12 worst requests.
- **A closed-loop client with only 16 threads** (M2 setup; ADR-0032 Context) caps concurrency. Above capacity, `target` mode just saturates the threads, so latency is bounded by Little's law and client-side p99 suffers coordinated omission.

**What to do instead:**
1. Run the coordinator on **t3.medium** (allowed, ≈ $0.02/h more) so the resource being scaled is the one that saturates.
2. Make "coordinator CPU or credits saturated" an explicit **stop condition in calibration** (task 7.1). An *inhibit* is not a *trigger*, so the brief's "not on CPU" still holds.
3. Base the scale-out decision on **replica-side latency** (the leader's request and commit-wait histograms already exist, per ADR-0031), or require it as confirmation of the coordinator p99.
4. Drop the read trigger for workload A, or give it a longer `for`.
5. Raise the minimum rate to about 100 req/s, or require a minimum *count* of requests above the edge.
6. Use more client threads (64–128) so the target rate, not the thread count, drives the load.
7. Restate the copy-rate requirement as a measurable budget (e.g. "copy adds ≤ X ms to the source's write p99").
8. To make the spike profile meaningful, consider a **warm standby set**: shard-3 booted and *pending* at session start, so a scale-out is only the migration. That's ≈ $0.06/h, ≈ $0.20 per 3-h session.

##### 4. Leader transfer relies on heartbeats to spread the epoch, so its own spec ("no 5xx") is impossible, and making it automatic by default enlarges the risk

**Where:** ADR-0036 Decision 2–3; spec `leader-placement` "Planned leader transfer", "Automatic rebalance of leaders"; ADR-0023 "Timeouts, together".

**What's wrong:**

- ADR-0036 says the old leader "learns the new epoch from its next heartbeat and answers 409 meanwhile". It can't answer 409 before it has learned the epoch. Worse, the **target** also learns that it is leader only from a heartbeat: ADR-0020 Decision 3, "announces them in its heartbeat answers", every 5 s.
- Writes released after the ≤ 2 s hold reach a target that may still answer 409.
- Once the target does act as leader, it needs a follower ack for W = 2. The other two replicas only re-point their pulls after their own heartbeat or the 7 s pull deadline. The 2 s commit wait expires first, so the client gets **504**.
- The requirement "no write SHALL get a 5xx because of a transfer" and its scenario ("writes resume on 1a within 2 s") therefore can't hold with this protocol, quite apart from weakness 1.
- **Automatic rebalance**, on by default and limited to one transfer per minute, fires whenever placement is uneven and every replica is live. That includes the middle of the final benchmark, where it adds 2 s write pauses to the latency curves being measured.
- A single 2 s hold puts ≈ 1.7 % of a 2-minute window's writes above 50 ms, so write p99 for that shard reads above threshold for the next 2 minutes. The guard "no scaling while a transfer is in progress" (spec `autoscaling` "Guards") ends with the transfer, not with the rate window.

**What to do instead:**
- Push the new epoch synchronously to all three replicas before releasing writes (see 1).
- Keep the automatic rebalance **off during benchmark profiles**, or move it to ADR-0036's runner-up D (see section 2).
- Extend the scaling guard to "no hold or transfer within the last rate window".

##### 5. Using an Auto Scaling group as a "launcher" brings AWS-side behaviour the design doesn't control, and the executor's state and guards can deadlock or leak spend

**Where:** ADR-0035 Decision, Risks; design "Scale-out, end to end"; tasks 5.3–5.4; spec `autoscaling` "Guards".

**What's wrong:**

- **"No scaling policies, so it never terminates on its own initiative" is not true.** An Auto Scaling group also runs **AZRebalance** and **ReplaceUnhealthy**.
  - If an AZ lacks t3.small capacity, the group launches in another AZ. Two instances then share an AZ ID, so the second becomes a duplicate replica ID, which the coordinator refuses.
  - When capacity returns, AZRebalance terminates an instance to rebalance, possibly the live one or the shard-3 leader, with no migration.
  - ReplaceUnhealthy starts an **empty** replica under the same ID automatically. ADR-0023 Decision 6 deliberately makes wipe-and-resync a manual step (`make rebuild-replica`).
- **CloudFormation drift.** The template sets `desired 0` (task 5.3), and the executor changes desired capacity outside CloudFormation. A stack update made while scaled out (any `make up-app` or template change in a session) resets desired capacity to 0 and **terminates shard-3 without a leave migration**.
- **Guard deadlock.** "No action while any replica is reported down" also blocks the scale-in that would retire a damaged shard-3, for example one whose instance the group failed to replace. The set then stays up, costing money, with no automatic way out.
- **State on the laptop.** The rate at the last scale-out and the cooldown are lost when the laptop restarts (ADR-0032 and ADR-0035 accepted downsides). The coordinator already persists the migration in its log. Put the triggering rate and the action's end time in that record, and the "lost state" downside disappears.
- **Time budget of session 3** (task 7.4). The three profiles each start from 2 sets. Between profiles: ≥ 10 min of N + a leave migration + a 10-min cooldown, ≈ 25 min each. Add gradual (≈ 30–40 min), spike (15 min + return), sustained (30 min), plus deploy, load and export. That is well beyond what fits in a ≈ 3-h session whose credentials expire, and the executor stops when they do (ADR-0035 Risks).

**What to do instead:**
- **Group processes:** suspend `AZRebalance` and `ReplaceUnhealthy` on the group and enable scale-in protection. Better still, use **three single-subnet groups (max 1 each)**, or launch-template `RunInstances` with explicit replica-ID tags, so each ID is fixed by its subnet.
- **Template:** leave `DesiredCapacity` out of it, and have `make up-app` refuse to run while shard-3 is not retired.
- **Guard:** allow scale-in when the down replica belongs to the leaving set and that set still has a leader and 2 live replicas.
- **Session plan:** plan session 3 explicitly, with a manual reset to 2 sets between profiles, or the sustained profile in its own session.

---

#### 2. Decisions where a rejected alternative is better

- **ADR-0036, option D (or B plus "wait for all expected replicas at first assignment") over C.** The two observed failures were (a) a late node at start-up and (b) a promotion. Waiting a bounded time for all three nodes before the *first* assignment, plus the tie-break, fixes (a) with no new safety-critical code. Case (b) is rare and is exactly what an approved `transfer_leader` proposal is for, which also gives the ops agent a real, non-duplicated job.

  Option C adds an automatic, epoch-changing operation to the leadership path (ADR-0036 accepted downside: "more code in the coordinator's leadership path, which M3 tested without it"). As written, that operation has the flaws in weaknesses 1 and 4, and its only benefit over D is "works with nobody watching", in a store that runs only while the author is present (ADR-0035). The ADR's claim that a transfer is "the same step" as cutover doesn't hold: cutover switches key ownership, while a transfer changes epochs and replica roles. So C doesn't save code by sharing with ADR-0033.

- **ADR-0035, execution: option C (change a CloudFormation parameter) deserves a second look, or at least B must adopt C's property.** C keeps the stack as the single source of truth for capacity, which removes the drift hazard in weakness 5. Its extra minute or two per action is small next to the ≈ 9–12 min the action already takes. If you keep B, take `DesiredCapacity` out of the template.

- **Not better, and worth saying in the defense:** ADR-0035's vertical runner-up (t3.small → t3.medium for replicas). M2 point 1 shows the write ceiling is one lock across `fsync` per leader. A t3.medium has the same 2 vCPUs and the same EBS behaviour, so vertical scaling of replicas wouldn't move it. Horizontal (a third leader) is the right unit. The real per-shard multiplier is group commit, which is correctly deferred for this milestone. The place where vertical *does* pay is the **coordinator** (weakness 3).

- ADR-0033's option 2 (switch writes first, read both shards) is correctly rejected. Option 1 is right once it is properly fenced.

---

#### 3. What milestone 5 should cover per the brief but doesn't yet

1. **A like-for-like comparison with the M2 baseline** (control question 8). M2 ran unthrottled closed-loop **workloads A and B**, without replication, on 2 shards. The M5 spec runs only target-rate **workload A** profiles. Add an unthrottled A and B run on 2 sets and on 3 sets with the same 16 threads. That is also the direct test of M2's claim that "more shards raise it linearly".
2. **Proof of data correctness in AWS during join and leave.** Task 7.2 records 5xx counts and the longest hold, which doesn't show that nothing was lost. Run a verifier writer during the lab join and leave (acknowledged writes read back, or go-ycsb `dataintegrity=true`). The local test (4.4) isn't evidence for the lab.
3. **The brief says "keep the old shard serving reads until migration completes".** Per-source cutover moves the reads of source 1's keys to the target before source 2 has cut over and before purge. That's defensible, but ADR-0033 should state and justify it (or switch reads for all sources together at the end), because a literal reading of the brief differs.
4. **"Number of active nodes over time."** The spec plots replica sets on the ring and live replicas. Also plot instance count (group InService instances or nodes known to the coordinator), since the brief says *nodes*.
5. **The ops agent should "consume your alerts".** There are no alerts for the situations the agent exists for: ADR-0036 explicitly says no alert for leaders sharing a node, and there is no "migration stalled" alert. Add info-level `LeadersColocated` and `MigrationStalled` alerts.
6. **Define what the agent's scale actions mean next to the autoscaler.** `scale_up`/`scale_down` either duplicate the deterministic rule or contradict it. Say which wins (e.g. agent scale proposals only when the autoscaler is blocked or off).
7. **Fix the agent's action list.** `resume_migration` is ill-defined because migrations already resume on their own (ADR-0033). The safe action for a stuck migration, `abort_migration` (before cutover), is missing.
8. **Make "the agent holds no credentials" true at the OS level, not just in code.** Run `kvops` as a separate OS user or container without `~/.aws`, with only the `status` key. Keep the proposal store somewhere the agent process can't write approvals.
9. **Treat the agent's tests as evaluations.** Task 6.2's "tests with recorded tool results → `scale_up`" either call the live model (flaky, and billed) or mock it (and then test nothing). Call them evaluations with expected kinds and a pass rate, and keep deterministic unit tests for the schema check, bounds, expiry and guards.
10. **Removal covers only shard-3.** The brief's "adding or removing shards" is met only for the added set; base sets can't leave. State it in the README and the ADR.
11. **The rate condition on scale-in (minor).** It goes beyond the brief's "below the threshold for N minutes". It's a good addition, but because it changes a brief-specified rule, defend it explicitly in the control-question notes.

## Verdicts

> **Proposed by Claude Code on 2026-11-28; confirmed by the author in chat ("Yes, apply
> them") and applied the same day** to ADRs 0032–0036, the design, the proposal, the specs
> and the tasks (all still **Proposed**). The one open item, a warm standby set for the
> spike profile, is not adopted; the design notes it as available on request. Claims were
> checked against the repo: `docs/m2/benchmark.md`
> points 1 and 3 (one lock across `fsync`; coordinator the next bottleneck), and
> `internal/shard/replica.go` (the leader keeps a commit point apart from its last
> appended LSN, so the follower pull does serve uncommitted records, ADR-0023).

| # | Point | Verdict | What changes |
| --- | --- | --- | --- |
| W1 | A hold at the coordinator doesn't fence in-flight writes | **Accept** | Fence at the data owner. Transfer: the coordinator tells the leader to stop accepting writes for epoch e; it finishes or fails its in-flight commits and reports its final LSN; the target reaches it; e+1 is persisted and pushed synchronously to all three replicas before writes are released. Cutover: the source leader writes a replicated *freeze* record for the moving keys and refuses writes to them after it; the target reaches it; then the switch. A test injects a write between the hold and the LSN read. ADR-0033, ADR-0036, both specs |
| W2 | The migration tail ships uncommitted records | **Accept** | The filtered tail serves only up to the source leader's commit point; L = the commit point taken before the first state page; `PURGE` and *freeze* records bump the record format to 3 (an M4 binary then refuses the log, ADR-0022's purpose); purge records batched (size stated). ADR-0033, design, spec |
| W3 | p99 at the coordinator may not close the loop | **Accept in part** | Accept: coordinator on **t3.medium**; coordinator CPU/credits saturated as a calibration **stop condition** (an inhibit, not a trigger); minimum rate 100 req/s per shard; 64–128 client threads with a target rate; the copy requirement restated as a budget on the source's write p99; leader-side commit wait as a confirming condition. Refute "drop the read trigger": keep it for workload B, but with `for: 5m`. **Author decides:** a warm standby set for the spike profile (≈ $0.20 a session; it shows migration speed, not boot speed). ADR-0032, ADR-0035, specs |
| W4 | Transfer spreads the epoch by heartbeat; "no 5xx" impossible; automatic by default | **Accept** | Synchronous epoch push (W1); scaling guard covers the last rate window after any hold; automatic transfer replaced as in section 2 below. ADR-0036, autoscaling spec |
| W5 | The ASG launcher has its own behaviour; drift; guard deadlock; laptop state; session time | **Accept** | Three single-subnet groups (max 1 each), so a replica ID is fixed by its subnet; `AZRebalance` and `ReplaceUnhealthy` suspended, scale-in protection on; `DesiredCapacity` left out of the template and `make up-app` refuses while shard-3 isn't retired; scale-in allowed when the down replica belongs to the leaving set and it still has a leader and 2 live replicas; the triggering rate and the action's end time persisted in the coordinator's migration record; session 3 split (gradual + spike, then sustained in session 4) with a manual reset to 2 sets between profiles. ADR-0035, ADR-0032, design, tasks |
| §2 | ADR-0036: D (or B + bounded wait at first assignment) over C | **Accept** | ADR-0036 becomes B + a bounded wait for all expected replicas before the first assignment + transfer only through an approved `transfer_leader` proposal (option D). Drops the "same step as cutover" claim |
| §2 | ADR-0035: C, or B adopting C's property | **Accept in part** | Keep B (the executor sets capacity), adopt C's property: capacity isn't in the template (W5) |
| §2 | Vertical for replicas wouldn't help; it does for the coordinator | **Accept** | Runner-up of ADR-0035 rewritten: vertical helps the coordinator, not leaders; that's why the coordinator goes to t3.medium (W3) |
| §3.1 | Like-for-like comparison with M2 | **Accept** | Unthrottled workloads A and B on 2 and on 3 sets, 16 threads, added to the final benchmark |
| §3.2 | Data correctness in AWS during join and leave | **Accept** | A verifier writer (acknowledged writes read back) runs during the lab join and leave |
| §3.3 | Per-source cutover vs "old shard serves reads until done" | **Accept** | ADR-0033 states it: each source's moving keys switch reads and writes together at that source's cutover, because the target then holds every committed write of those keys; serving them from the source any longer would serve stale data |
| §3.4 | Active *nodes* over time | **Accept** | Instances known to the coordinator plotted with sets and live replicas |
| §3.5 | Alerts for what the agent handles | **Accept** | Info-level `LeadersColocated` and `MigrationStalled` alerts |
| §3.6 | Agent scale actions vs the autoscaler | **Accept** | The agent proposes `scale_up`/`scale_down` only when the autoscaler is off or blocked, and says which guard blocked it |
| §3.7 | `resume_migration` ill-defined; `abort_migration` missing | **Accept** | Replaced by `abort_migration` (before any cutover) |
| §3.8 | No credentials at the OS level | **Accept** | `kvops` runs in a container without `~/.aws` or the admin key, only the `status` key; approvals written by the separate command to a store the agent can't write |
| §3.9 | Agent tests are evaluations | **Accept** | Task 6.2 split: deterministic unit tests (schema, bounds, expiry, guards) and an evaluation set with expected kinds and a pass rate, run on request (billed) |
| §3.10 | Only shard-3 can leave | **Accept** | Stated in ADR-0033 and the README |
| §3.11 | Scale-in rate condition goes beyond the brief | **Accept** | Defended in the control-question notes |
