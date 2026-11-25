# M3 AI design review

> The critique must come from a **fresh AI session with no project context**, not from
> the session that drafted the design (course rule; CLAUDE.md rule 5). This file holds the
> prompt, the transcript and my verdicts.

**How it was run:** Claude Code (Opus 5.5) started a separate session with no access to
the drafting conversation, in read-only (plan) mode, on 2026-11-25. It received only the
prompt below and read the draft from the working tree of branch `m3-replication` (status
Proposed, uncommitted). Its final message is saved below verbatim.

## Prompt

```text
You are a senior cloud architect reviewing a student's design. Be critical: your job is
to find weaknesses, not to praise.

Context: a course project to build a small DynamoDB-like key-value store over five
milestones (sharding, replication with quorum reads, monitoring, autoscaling on p99). This
is milestone 3: replication. Each shard becomes a replica set, writes go through a log to
followers, and reads use quorums with R + W > N. It runs in a restricted AWS Academy
sandbox (no IAM role creation, instances limited to t3.medium, MSK/MQ/SQS/Kinesis denied,
a $60 credit with about $36 left).

Read only these files in the repository (do not change any file and make no AWS calls):
- the "Milestone 3" section of docs/course/capstone-brief.md
- openspec/changes/m3-replication/proposal.md, design.md, tasks.md and specs/*/spec.md
- docs/adr/0020-replication-model-and-placement.md, 0021-n-w-r-values.md,
  0022-log-structure-and-ordering.md, 0023-replica-sync.md
- for background on what exists from M2: docs/adr/0011-hash-and-virtual-nodes.md,
  0013-shard-storage-and-membership.md, 0014-app-infrastructure.md,
  0018-log-damage-and-failed-writes.md

Give:
1. The 5 most serious weaknesses, ranked: what is wrong, why it matters for later
   milestones (M4 conflict resolution and monitoring, M5 autoscaling on p99), and what you
   would do instead.
2. Any decision where a rejected alternative is actually better.
3. Anything missing that milestone 3 should cover per the brief.
Be specific and cite the document and section.

Your final message must be the full review as Markdown, starting with a heading
"Independent AI design review — M3 (m3-replication)" and a line stating that you are a
fresh session with no prior project context, the model you are, the date, and exactly
which files you read. That final message is the deliverable: the author will paste it
verbatim into docs/m3/ai-review.md, so do not write it to a file yourself.
```

## Transcript

### Independent AI design review — M3 (`m3-replication`)

I'm a fresh Claude Code session with no prior project context. Model: Claude Opus 5.5 (`claude-opus-5-5`). Date: 2026-11-25.

**Files read (working tree, branch `m3-replication`), and nothing else:**
- `docs/course/capstone-brief.md`, the "Milestone 3" section only (lines 97–125)
- `openspec/changes/m3-replication/proposal.md`, `design.md`, `tasks.md`
- `openspec/changes/m3-replication/specs/replication/spec.md`, `specs/quorum-reads/spec.md`, `specs/replication-benchmark/spec.md`
- `docs/adr/0020-replication-model-and-placement.md`, `0021-n-w-r-values.md`, `0022-log-structure-and-ordering.md`, `0023-replica-sync.md`
- Background: `docs/adr/0011-hash-and-virtual-nodes.md`, `0013-shard-storage-and-membership.md`, `0014-app-infrastructure.md`, `0018-log-damage-and-failed-writes.md`

**Disclosure:** the Claude Code harness put two things in my context automatically. One is the repository's `CLAUDE.md`, which holds the process and AWS rules. The other is a `git status` snapshot. I did not open any other file, and I did not read the shared notes folder of the session that started me. No files were changed and no AWS calls were made.

---

#### 1. The five most serious weaknesses (ranked)

##### 1.1 Promoting a follower can lose committed writes and leave replicas silently diverged

**What's wrong.** Four separate problems combine here.

1. **The promotion rule is too weak.** ADR-0020 §Decision accepts a promotion if the target "has the highest applied LSN among the **live** replicas". The `replication` spec, Requirement "Leader loss and promotion", says the same. With W = 2, a committed write sits on the leader plus *one* follower, and it can be either follower. If the leader is down, only both followers together are sure to hold every committed write. Concrete case:
   - Leader A commits LSN 101 on A + C.
   - C crashes, then A crashes.
   - B (LSN 100) is the only live replica. It has the highest LSN among live replicas, so promoting it is accepted.
   - Committed write 101 is lost. ADR-0020 rates exactly this risk High, and its listed mitigation doesn't prevent it.
2. **LSNs are reused, and a returning follower diverges permanently.** B, now leader, gives a *different* write LSN 101. C comes back and pulls `after=101` (ADR-0023 step 4), so it never fetches B's 101. From then on C serves a value that B never had, under the same version number. The coordinator's "newest LSN wins" rule (`quorum-reads` spec) can't tell the two apart. With Merkle trees deferred (proposal §Non-Goals), nothing ever detects it.
3. **There is no fencing or leader epoch.**
   - ADR-0020 says the old leader "is rebuilt empty", but the spec scenario "Promotion accepted" only says the former leader "is told it's a follower". Nothing says who wipes its disk or what forces that to happen. ADR-0022's mitigation for the "same LSN, different record" risk depends on that wipe.
   - Followers find out who the leader is from where? Neither the design nor the ADR says how a follower learns the new leader's address or stops pulling from the old one.
4. **Who the leader is lives only in the coordinator's memory.** The design §Membership says replicas report their `role` in each heartbeat, and the coordinator "keeps, per shard, the leader". Nothing says that assignment is saved to disk. Picture this: after a promotion, the coordinator restarts and the old leader comes back reporting `role: leader`. Two replicas now claim to lead, and the design has no rule for choosing. Task 6.1 also makes `ExpectedShards` become "replicas per shard". If the coordinator's start-up gate (ADR-0013 Risks) waits for every replica, one dead replica blocks the whole store after a coordinator restart.

**Why it matters later.** M4's topic is "handling failures". These are exactly the failure paths the M4 monitoring and conflict-resolution work will exercise. Divergence that M3 creates without any trace will show up in M4 as unexplained conflicts. M5's ops agent would also be automating a promotion procedure that is unsafe today.

**What I'd do instead.**
- **Add a leader epoch (term).** The coordinator increments it on every promotion and saves it to disk next to its table registry. Each record and each pull carries `(epoch, LSN)`.
- **Require both followers for promotion when the leader is down.** That is a majority of N = 3 that reports, so it includes at least one copy of every committed write. Read their LSNs fresh at promotion time; a heartbeat value can be up to 5 s old.
- **Truncate on rejoin.** A rejoining replica sends `(epoch, lastLSN)`. The leader answers with the last LSN where their logs agree, and the replica cuts its log back to that point. This makes the "rebuilt empty" step automatic and enforced.
- **Answer pulls only as the current leader.** A replica that is not the leader for the current epoch replies 409 to pulls and to writes.
- **Save the leader assignment** in the coordinator's log.

##### 1.2 A bare per-shard LSN can't serve as a version, and M4 conflict resolution has nothing to build on

**What's wrong.**
- ADR-0022 §Decision defines a key's version as "the LSN of its last write". That number is unique only while the leader never changes (see 1.1). It carries no notion of who wrote it or when.
- ADR-0020 names leaderless replication as the runner-up, to "reconsider in M4". If M4 does that, a per-shard counter is meaningless when several replicas accept writes at once.
- The record format `length | checksum | LSN | op | key | value` (ADR-0022) has no spare field and no format-version byte. So M4 would need yet another log-format break.
- Nothing detects divergence. Merkle trees are deferred, and no cheaper check is proposed.

**Why it matters.** M4's "conflict resolution" needs a version that can be compared across writers and leader changes (last-writer-wins with a timestamp, or vector clocks). M4 monitoring also wants a "replicas diverged" signal, and there is no way to compute one.

**What I'd do instead.**
- Make the version `(epoch, LSN)` now.
- Add a format-version byte, plus a hybrid logical clock (HLC) timestamp field per record, ready for M4.
- Add a **hash chain**: each record stores `H(previous hash ‖ record)`. A follower checks the chain on every pull. That turns divergence into an error at the moment it happens, for about 8 bytes per record and a few lines of code. It covers most of what the optional Merkle tree would give, for much less work.

##### 1.3 Writes that never committed (or that returned 503) are visible to reads, and "retrying is safe" isn't true

**What's wrong.**
- Neither ADR-0023 nor the design says **when the leader applies a write to its in-memory map**: before the commit wait or after it. If it applies before, an R = 2 read that includes the leader returns a value on one disk only. That value is lost if the leader dies (1.1), so a client can read a value and then find it gone.
- ADR-0023 §Accepted downsides admits a 503 write "may still become visible later". It answers that a retried put is "a harmless overwrite" (ADR-0021 Risks, ADR-0023 step 3). That only holds with a single writer. Example:
  - Client 1's put X times out with 503.
  - Client 2 writes Y.
  - Client 1 retries X and overwrites Y.

  That is a lost update caused by the retry advice itself. Deletes interleaved with retries make it worse.
- A 503 doesn't mean "not written", yet the public API is unchanged (design Decision 5). Clients therefore can't tell "failed" from "unknown outcome".

**Why it matters.** Lost updates and phantom reads are what M4's conflict-resolution work will look for. If the M3 write semantics are vague, the M4 analysis has no reference point. For M5, every 2-second timeout inflates p99 and feeds the autoscaling signal.

**What I'd do instead.**
- State the rule: the leader applies to its map only after commit.
- Followers apply everything they pull. Each pull response should carry the leader's current commit LSN, so reads can tell committed data from data still waiting.
- Return a distinct status for a write whose outcome is unknown (for example 504, or 503 with a header meaning "outcome unknown").
- Add an optional client request ID that the leader removes duplicates of, or at least drop the "retry is safe" claim.

##### 1.4 Promised M2 work is missing from the M3 log design, and the write path is the slowest version possible

**What's wrong.** ADR-0018, which is Accepted, deferred three things explicitly to M3:
- **group commit**: "record versions/LSNs and group commit come with M3";
- **a header checksum**: "M3's record format can add a header checksum";
- **replica re-sync of a damaged log**: "M3's replicas will let it re-sync instead".

ADR-0022 brings in LSNs only. Its record format doesn't say whether the checksum covers the length and LSN fields. That matters more now: a corrupted LSN field turns into a wrong version and wrong resync offset on every replica. No document says whether a follower **verifies checksums on records it pulls**. No document describes how a replica whose log is damaged in the middle (ADR-0018 says it "refuses to start") gets rebuilt from the leader.

Meanwhile, per ADR-0023, each write costs, one after another:
1. a leader fsync,
2. the long-poll return,
3. a follower fsync,
4. the follower's *next* request (the ack).

Without group commit, the leader fsyncs once per write while waiting writes pile up behind it.

**Why it matters.** M5 autoscales on p99. A write p99 made of two fsyncs plus about 1.5 cross-AZ round trips, with no batching, will be the dominant signal. Adding instances can't lower it, so the autoscaler will react to a latency it can't fix.

**What I'd do instead.**
- Put header checksum and group commit (fsync once per batch on both leader and follower) into ADR-0022. Mark ADR-0018's deferrals as resolved.
- Specify that followers verify checksums and the hash chain (1.2) on pull.
- Add a "rebuild damaged replica from the leader" scenario to the `replication` spec.
- Consider a separate ack call, or a follower ack sent right after fsync, so the commit doesn't wait for the follower's next request.

##### 1.5 Read scaling, observability and topology aren't ready for M4 monitoring or M5 autoscaling

**What's wrong.**
- **Read capacity.** R = 2 with pair rotation (`quorum-reads` spec) means each client read costs two replica reads. Three replicas give only about **1.5×** the read capacity of one shard, not 3×. Read p99 is the *maximum* of two replicas, and nothing routes around a slow one: no hedged reads, no least-outstanding-requests choice. ADR-0021 Risks lists "a slow follower drags every read" and its only mitigation is "measured".
- **The single coordinator** now sends every read to two replicas, holds all leader state, and is still one unreplicated instance (ADR-0014). The proposal claims durability ("data survives the loss of a replica"), but the coordinator's table registry (ADR-0013 §Decision) has no replica at all.
- **No replication metrics are specified**: follower lag (leader LSN − follower LSN), commit-wait time, 503s caused by commit timeouts, open long-polls. M4 needs these as monitoring inputs.
- **The topology doesn't fit autoscaling.** Six stateful instances with stable IDs and fixed AZ placement don't fit ADR-0014's plan that "M5 will replace the shard instances with an Auto Scaling group". The scaling unit is now a whole replica set of three instances, and the lab's instance cap is still unknown (design §Open Questions).
- **The fallback breaks the brief.** ADR-0020 Risks says that if the cap is below 8, the store falls back to "2 replicas". The brief requires "3+ replicas". N = 2 with W = 2 / R = 2 also loses the one-replica-down availability that the design §Goals promises.

**What I'd do instead.**
- Make W configurable (1 or 2) and add hedged reads: send to 2, and add a third request after the 95th-percentile delay.
- Add the lag and commit-wait metrics to the specs now.
- Write down how replica sets map to Auto Scaling groups for M5 (for example one group per AZ, or a fixed replica set per scaling step).
- Replace the fallback with co-location (see 2.2), not a smaller N.

---

#### 2. Decisions where a rejected alternative is actually better

##### 2.1 ADR-0020, leader loss: automatic promotion run by the coordinator beats manual promotion

Option C is framed only as "Raft-like" and rejected for "split brain if fencing is wrong". But the design already has a single, central authority: the coordinator. Promotion run by the coordinator doesn't need votes or a consensus protocol. Its outline:
- the epoch from 1.1;
- wait until both followers report;
- choose the higher `(epoch, LSN)`;
- bump the epoch and announce it in the heartbeat reply.

That is the classic "configuration master" pattern from GFS and chain replication. It costs roughly what the manual endpoint already costs, plus the epoch the design needs anyway. It removes a human from the write-availability path, which M4's failure handling and M5's ops agent will want. It is also *safer* than the current manual rule, because the code enforces the majority check instead of an operator.

##### 2.2 Placement: three instances each hosting one replica of each shard beats six single-shard instances

This option isn't even listed in ADR-0020. With three instances, one per AZ, each running one replica of shard 1 and one of shard 2:
- the brief's "3+ replicas per shard" still holds, and so does tolerance of one lost AZ;
- the shard instance count halves (5 total instead of 8 with coordinator and client), on a $36.6 budget;
- the unknown instance-cap risk largely disappears;
- the leader for shard 1 and the leader for shard 2 can sit on different instances to spread write load.

It's also closer to how Dynamo and Cassandra nodes actually work. The cost: noisy neighbours on a t3 instance. Document that in the benchmark notes.

##### 2.3 ADR-0021: the runner-up isn't better, but it should stay available as a setting

W = 2 / R = 2 is the right default, and the ADR's reasoning against W = 1 / R = 3 is sound. Keep W = 1 / R = 3 as a **configuration**, not just a rejected option. One benchmark run with W = 1 answers the control question "How do write-ack strategies (sync vs async followers) affect tail latency and durability?" with measured numbers instead of argument.

---

#### 3. What milestone 3 should cover per the brief but doesn't (or only partly)

1. **Sync vs async acks, measured.** The control question asks how they affect tail latency. Nothing in `replication-benchmark/spec.md` produces data to answer it. Add the W = 1 run from 2.3.
2. **The p99 explanation needs a degraded run.** "How did p99 change after replication, and why exactly?" needs at least one run with a follower stopped and one with a slow follower. Without those, the explanation is a guess. The benchmark spec currently requires "no failed operations" only on a healthy cluster.
3. **Failure modes and the staleness bound aren't written down.** The brief asks "What failure modes lead to temporary inconsistency? How does your design bound staleness?" The design has an availability table but no list of failure modes and no stated bound. Scenarios 1.1 and 1.3 are exactly the "temporary inconsistency" cases. As designed, though, some of that inconsistency is *permanent*.
4. **CP/AP scenarios are only implied.** The brief asks for a concrete scenario. One sentence ("CP behaviour for writes", ADR-0020) isn't an analysis. Two cases need walking through: a partition between the coordinator and a leader, and a split between AZs.
5. **"Think about delivery order and versions"** is covered within a shard. It isn't covered across leader changes (1.1, 1.2), which is where it matters.
6. **Optional anti-entropy.** Deferring Merkle trees is fine. Having no divergence detection at all isn't, given 1.1. The hash chain from 1.2 is a cheap, honest answer to the optional task.
7. **Followers rejoining after the leader changes.** The brief says followers "upon restart resync only the relevant log range". The design defines that range only against a leader that never changed.
8. **Smaller inconsistencies to fix before acceptance:**
   - ADR-0013's header says it is "superseded in part by ADR-0022", but ADR-0022 has no `Supersedes` line.
   - The timeouts aren't reconciled with each other: a 5 s long-poll, a 2 s commit timeout, and a 15 s heartbeat failure detector (3 × 5 s). The design doesn't say what deadline a follower uses on a long-poll to a leader whose packets are silently dropped.
   - The proposal says "8 instances" while ADR-0014 states the M2 shard instances are t3.small with `unlimited` credits. Running six of them during a benchmark can create surplus-credit charges, which the cost estimate doesn't include.

## My verdicts

Proposed by the drafting session; agreed by the author in chat on 2026-11-25.
*(Author: rewrite the arguments in your own words before the defense.)*

### Argued at the defense

| # | AI point | Verdict | Argument | Change to make |
| --- | --- | --- | --- | --- |
| 1 | 1.1 + 2.1: promotion can lose committed writes; let the coordinator promote automatically with an epoch | **Accept** | Right, and it's a real bug: with W = 2 a committed write is on the leader and *one* follower, so a promotion that sees only one live follower can pick the one without it. Only both followers together are guaranteed to hold every committed write. Once the epoch and the "both followers" rule are in the code, the coordinator can apply them itself; a human adds delay, not safety | ADR-0020: leader epoch kept on the coordinator's disk; automatic promotion only when both followers report fresh LSNs, highest `(epoch, LSN)` wins; replicas with an older epoch refuse writes and pulls; a returning replica truncates its log to the last record the leader confirms. Spec scenarios for each |
| 2 | 1.4: group commit, promised by ADR-0018, is missing | **Refute for M3** | The point of the M3 benchmark is to measure what replication costs compared with M2. Adding group commit at the same time changes two things at once, and the M2 vs M3 numbers would no longer say what replication costs. Group commit is a p99 optimisation, so it belongs with M5's p99 work, measured on its own. ADR-0018's promise is moved explicitly, not forgotten | ADR-0022 records the deferral to M5 with this reason |
| 3 | 1.2: add an HLC timestamp per record now, ready for M4 | **Refute** | M4 decides how conflicts are resolved; fixing a clock format now presumes that answer. The record gets a format-version byte instead, which is what lets M4 add any field later without breaking old logs | ADR-0022: version byte, version = `(epoch, LSN)`; no HLC |

### Every other point

| AI point | Verdict | Change to make |
| --- | --- | --- |
| 1.1: LSNs reused after a leader change; leader assignment only in memory; start-up gate waits for every replica | Accept | Epoch (verdict 1); assignment persisted in the coordinator's log; the start-up gate waits for 2 of 3 replicas per shard |
| 1.2: no divergence detection | Accept | Hash chain: each record stores a hash of the previous record and itself; a follower checks it on every pull. Covers the optional anti-entropy task more cheaply than a Merkle tree |
| 1.3: leader applies before commit; "retry is safe" is wrong; 503 hides "unknown outcome" | Accept | The leader applies to its map only after commit. A commit timeout returns **504** ("outcome unknown"), not 503; the public API documents it. The "retry is safe" claim is removed |
| 1.3: client request IDs for de-duplication | Refute | Conflicting writers are M4's topic; M3 states that a retried write can overwrite a newer one |
| 1.4: header checksum; followers verify pulled records; rebuild of a damaged replica | Accept | The checksum covers the whole record including length and LSN; followers verify checksum and hash chain; a replica with mid-log damage is wiped and resynced from the leader (spec scenario) |
| 1.4: a separate ack call right after the follower's fsync | Refute | The follower sends its next pull immediately after fsync, and that pull *is* the ack: a separate call would take the same round trip |
| 1.5: hedged reads | Refute for M3 | Measure first: the benchmark shows whether a slow replica dominates read p99. Hedging is a p99 tool for M5 |
| 1.5: replication metrics | Accept | Each replica exposes last LSN, lag behind the leader and commit-wait time on its health endpoint; the coordinator counts 504s |
| 1.5: single coordinator, registry not replicated | Accept as a known limit | Stated in the design's failure modes; the coordinator's scaling is a control question of M2 and stays out of M3 |
| 1.5: autoscaling unit for M5 | Defer to M5 | One line in ADR-0020: the scaling unit is a replica set |
| 1.5 + 2.2: fallback to 2 replicas breaks the brief; co-locate replicas | Accept | 3 shard instances, one per AZ, each running one replica of each shard (two containers, two ports); 5 instances in total instead of 8. Leaders of the two shards on different instances |
| 2.3 + §3.1: W = 1 as a setting, measured | Accept | W is a setting (1 with R = 3, or 2 with R = 2); the benchmark adds one W = 1 run |
| §3.2: degraded benchmark run | Accept | One run with a follower stopped |
| §3.3–3.5, 3.7: failure modes, staleness bound, CP/AP scenarios, ordering across leader changes, rejoin after a leader change | Accept | A "Failure modes" section in the design: each mode, how long the inconsistency lasts, CP/AP behaviour for a coordinator–leader partition and an AZ split |
| §3.8: ADR-0022 lacks its Supersedes line; timeouts not reconciled; t3 credit surplus not in the cost | Accept | Supersedes line added; one timeout table (long-poll 5 s, follower deadline 7 s, commit 2 s, failure detection 15 s); credit surplus in the M3 cost estimate |
