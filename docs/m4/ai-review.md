# M4 AI design review

> The critique must come from a **fresh AI session with no project context**, not from
> the session that drafted the design (course rule; CLAUDE.md rule 5). This file holds the
> prompt, the transcript and the verdicts.

**How it was run:** Claude Code (Opus 5.5) started a separate session with no access to
the drafting conversation, in read-only (plan) mode, on 2026-11-27, at the author's
request ("починай роботу над усим"). It received only the prompt below and read the draft
from the working tree of branch `m4-monitoring` (status Proposed, uncommitted). Its final
message is saved below verbatim, with its headings moved two levels down.

## Prompt

```text
You are a senior cloud architect reviewing a student's design. Be critical: your job is
to find weaknesses, not to praise.

Context: a course project to build a small DynamoDB-like key-value store over five
milestones (sharding, replication with quorum reads, monitoring and failure handling,
autoscaling on p99). This is milestone 4: monitoring, conflict resolution, and an MCP
server for AI agents. The store already has 2 shards, each a replica set of 3 with a
single leader (W = 2, R = 2), coordinated by one coordinator. It runs in a restricted AWS
Academy sandbox (no IAM role creation, CloudWatch custom metrics, dashboards and X-Ray
denied, instances can't send metrics or logs to CloudWatch, private subnets have no
internet, instances may read Secrets Manager, a $60 credit with about $32 left).

Read only these files in the repository at its current working tree (branch
`m4-monitoring`; the M4 files are uncommitted drafts). Do not change any file and make no
AWS or network calls:
- the "Milestone 4" section of `docs/course/capstone-brief.md`
- `openspec/changes/m4-monitoring/proposal.md`, `design.md`, `tasks.md` and `specs/*/spec.md`
- `docs/adr/0025-metrics-and-slis.md`, `0026-slo-values.md`,
  `0027-mcp-boundaries-and-auth.md`, `0028-record-versions-and-lww.md`,
  `0029-monitoring-stack-and-logs.md`
- for background on what exists from M3: `docs/adr/0020-replication-model-and-placement.md`,
  `0021-n-w-r-values.md`, `0022-log-structure-and-ordering.md`, `0023-replica-sync.md`,
  and `docs/m3/benchmark.md`

Give:
1. The 5 most serious weaknesses, ranked: what is wrong, why it matters (for operating and
   debugging the store, and for M5 autoscaling on p99 and rebalancing), and what you would
   do instead.
2. Any decision where a rejected alternative is actually better.
3. Anything missing that milestone 4 should cover per the brief.
Be specific and cite the document and section.

Your final message must be the full review as Markdown, starting with a heading
"Independent AI design review — M4 (m4-monitoring)" and a line stating that you are a
fresh session with no prior project context, the model you are, the date, and exactly
which files you read. That final message is the deliverable: the author will paste it
verbatim into `docs/m4/ai-review.md`, so do not write it to a file yourself.
```

## Transcript

### Independent AI design review — M4 (m4-monitoring)

I am a fresh session with no prior project context, running as Claude Opus 5.5 (`claude-opus-5-5`), on 2026-11-27. I read only these files, in the working tree of branch `m4-monitoring` (the M4 files are uncommitted drafts): the "Milestone 4" section of `docs/course/capstone-brief.md` (I also skimmed its Milestone 5 section to check what M5 needs); `openspec/changes/m4-monitoring/proposal.md`, `design.md`, `tasks.md`, `specs/observability/spec.md`, `specs/record-versions/spec.md`, `specs/api-keys/spec.md`, `specs/mcp-server/spec.md`; `docs/adr/0025-metrics-and-slis.md`, `0026-slo-values.md`, `0027-mcp-boundaries-and-auth.md`, `0028-record-versions-and-lww.md`, `0029-monitoring-stack-and-logs.md`; and, for M3 background, `docs/adr/0020-replication-model-and-placement.md`, `0021-n-w-r-values.md`, `0022-log-structure-and-ordering.md`, `0023-replica-sync.md` and `docs/m3/benchmark.md`. I made no AWS or network calls and changed no repository file. Statements about Prometheus, Alertmanager, Promtail and MCP clients come from my general knowledge, not from the repository, and should be checked.

---

#### 1. The five most serious weaknesses (most serious first)

##### W1. The metrics can't show where latency comes from. That misses part of the brief's M4 monitoring task and leaves M5 without the signal it needs to scale

**What's wrong**
- The brief (M4 §1) asks for "latency (p50, p95, p99), throughput, error rate for coordinator **and shards**". In the design's metrics table (`design.md` "Metrics", ADR-0025 "Collected"), the replicas have **no request-latency histogram and no error counter**. They have only `kv_commit_wait_seconds` (leaders only) and `kv_replica_reads_total`, which is a counter. So the shards' p50/p95/p99 and error rate don't exist.
- The coordinator's histogram is `kv_request_duration_seconds{op}`, which has **no `shard` label**, while `kv_requests_total` does. So even at the coordinator you can't get a per-shard p99. ADR-0025's own risk table lists `shard` as an allowed fixed label. Adding it costs about 5 ops × 2 shards × 14 buckets = 140 series, which is nothing.
- There are no host or runtime metrics: no CPU, steal, disk fsync latency, CPU credits, goroutines or GC. `docs/m3/benchmark.md` names two unexplained causes for the read tail: neighbours on the same host ("Noisy neighbours") and the store holding its lock during fsync ("Not verified; a profile … would settle it (M5)"). The M4 metric set can't confirm or rule out either one.
- R = 2 makes each read take as long as the slower of two replicas (ADR-0021, benchmark "Reads"). But nothing records which replica was the slow one, so one bad replica can't be told apart from a whole shard under load.

**Why it matters**
M5 (brief §1) scales "when p99 latency exceeds a threshold". Per ADR-0020 the unit that scales is a replica set. To decide which set to scale, or whether to scale at all rather than replace one noisy replica, you need p99 per shard and service time per replica. With only an aggregate coordinator p99, the M5 autoscaler would scale blindly. Also, in this sandbox CloudWatch custom metrics are denied, so an EC2 Auto Scaling target-tracking policy on p99 can't work. M5's scaling signal **has to** come from this Prometheus. That makes the `monitor` instance a dependency of the system's control loop, yet ADR-0029 treats it as optional, running only during sessions, keeping 24 h of data, and losing everything at `make down-app`. None of the M4 documents says how M5 will read p99, or that the monitor is about to become critical.

**What I'd do instead**
- Add `shard` to `kv_request_duration_seconds`.
- Add `kv_replica_request_duration_seconds{op,shard,replica}` and `kv_replica_errors_total` on every replica.
- Export Go runtime metrics (`runtime/metrics`), and run `node_exporter` on every node, delivered through S3 like Promtail.
- Have the coordinator log how long each replica took for each read (see W3), so one slow replica shows up.
- Add a paragraph to ADR-0029, or a short new ADR, saying that the monitor's Prometheus is where M5 will read p99, that it will therefore stop being optional in M5, and that its TSDB is snapshotted to the artifacts bucket before teardown. M5 also needs the "before/after shard sizes" and "number of active nodes over time" (brief M5 §2, §4), which can't be shown from 24 h of data that is thrown away at teardown.

---

##### W2. LWW with client timestamps doesn't deliver its main promise, and it adds new correctness hazards

**What's wrong**
1. **The default retry still overwrites the newer write.** `proposal.md` and `design.md` ("Versions and last-write-wins") say "a late retry can't overwrite a newer write, whatever order they arrive in". That holds only if the client stamped its **first** attempt with its own clock (ADR-0028: "it keeps the stamp of its first attempt"). A client that sends no `X-KV-Timestamp`, which includes go-ycsb, the smoke test and, as far as the spec says, the MCP server, gets a fresh leader stamp `max(now, last+1)` on the retry. That stamp is above B's, so A's retry overwrites B: the exact M3 problem ADR-0021 recorded. When every write is stamped by the server, the LWW check can never reject anything. So by default "last-write-wins" is really "last arrival wins".
2. **The only way to get the guarantee brings in client clocks.** A client 4 s behind loses every race for 4 s. A client 4.9 s ahead is accepted and pushes the leader's HLC forward (the HLC receive rule).
3. **The 5 s bound is ambiguous and can ratchet.** The record-versions spec says "more than 5 seconds ahead of the leader's clock". If "the leader's clock" means the HLC, which has just moved forward, then every accepted future stamp raises the bound for the next one, and stamps can drift without limit. The spec must say the bound is measured against the leader's **physical** clock.
4. **A write is reported superseded by a write that may never commit.** ADR-0028 compares against "the key's latest record (committed or not)". Take write X with stamp 10, appended but not yet committed, and write Y carrying client stamp 9. Y is answered 200 + `X-KV-Superseded` right away. Then X times out with 504, the leader dies, and X is truncated (ADR-0023 §5). Y's client was told a newer write won, but neither X nor Y exists. That is an acknowledged write silently lost.
5. **There are two version notions, and the one in the ETag breaks in M5.** `ETag`/`If-Match` use `(epoch, LSN)` and LWW uses the HLC. ADR-0028 itself says `(epoch, LSN)` "can't be compared" across shards. When M5 moves a key, an `If-Match: "1.40"` read from the old shard is checked against the new shard's unrelated log. It may match by accident, which lets a lost update through, or fail by accident with a 412.
6. **The HLC has no tie-breaker, and migration can make new writes look old.** Two leaders can produce the same 64-bit stamp. Worse: if the source leader's clock is ahead (up to 5 s, by design), migrated records carry "future" stamps. The target leader then stamps new writes below them, and the new writes lose to old migrated values.

**Why it matters**
This is the M4 correctness feature, and ADR-0028 presents the HLC as what M5's key migration will compare. As written, the M4 demo scenario only works with a hand-crafted header. The defects will show up in M5 rebalancing, where they are hardest to debug.

**What I'd do instead**
- Have the leader stamp every write, and return the stamp on **every** write response, **including the 504** (the leader appended the record before the commit timed out, so it knows the stamp). A retry sends back the server-issued stamp. That gives idempotent, LWW-safe retries without client clocks.
- Either drop arbitrary client stamps or keep them strictly bounded against the physical clock.
- Answer "superseded" only after the superseding record has committed. Wait for its commit, or return 504 if it times out.
- Make the version `(HLC, replica-set ID)`, use it as the ETag, and make migration apply the HLC receive rule on the target.
- Fix the claim in the proposal and design text.

---

##### W3. The monitoring can go blind exactly when the system is down, and the detection time is optimistic

**What's wrong**
- **A dead coordinator is invisible to the SLI.** Availability and p99 are measured only by the coordinator itself (ADR-0025). The coordinator is a single point of failure (ADR-0020, "Accepted downsides"). If it dies, the SLI series disappear instead of reporting 0% availability. The `kv_replica_up`/`kv_shard_has_leader` series are also published by the coordinator, so `ReplicaDown` and `ShardWithoutLeader` **can't fire either**: a rule over a missing series returns nothing. What remains is `ScrapeFailing` at severity **warning**. So a total outage pages as a warning.
- **The monitor can't watch itself.** ADR-0029 accepts "Telegram silence is the only sign". Silence is indistinguishable from a healthy system.
- **The "about 35 s" detection figure leaves out rule evaluation and delivery.** The design's sum (15 s heartbeat + ≤ 5 s scrape + 15 s `for`) assumes rules are evaluated every 5 s, but `evaluation_interval` isn't specified anywhere. Prometheus defaults to **1 m**, and Alertmanager's default `group_wait` is **30 s**. With defaults, alert delivery can take close to two minutes, which breaks the spec's own scenario ("reaches the Telegram chat within 60 seconds"). The `for: 15s` also waits a second time on a signal the 15 s heartbeat already waited for.
- **The 504 alert and the latency alerts are noisy at low traffic.** `WritesNotCommitted` fires on any single 504. Outside benchmark runs the request rate is near zero, so a 5-minute p99 is computed from a handful of samples and one slow request trips `ReadLatencyHigh`.

**Why it matters**
For operating the store, the worst failure, a coordinator outage, gets the weakest signal. For the control question "how do you detect a node failure, how long does it take?" the design states a figure (35 s, "Measured in AWS") before measuring it, using configuration it hasn't specified. For M5, an autoscaler or ops agent fed by these alerts would read the absence of alerts during an outage as "healthy".

**What I'd do instead**
- Add a blackbox probe (the `blackbox_exporter` image, or a small Go prober on the monitor) that does an authenticated GET/PUT through the coordinator every 5 s. Count its result as part of the availability SLI, so the SLI is also measured from outside.
- Make `up{job="coordinator"} == 0` **critical**, and add `absent(kv_replica_up)` rules.
- Add an always-firing `Watchdog` alert routed to a dead-man's switch. If an external service isn't possible, at least use a CloudWatch alarm on the monitor instance's `StatusCheckFailed`, which ADR-0029 implies is still allowed ("only EC2's own metrics and alarms").
- Pin `evaluation_interval: 5s`, `group_wait`, `group_interval` and `repeat_interval` in the spec.
- Drop `for` on `ReplicaDown`, or cut it to one evaluation.
- Gate the latency alerts on a minimum request rate.
- Report the detection time as a measured distribution from task 8.2, not as a design figure.

---

##### W4. The MCP boundary treats `write` as harmless, but in this store an overwrite destroys data just like a delete

**What's wrong**
- ADR-0027 (Decision 5) gives the default MCP key `read` + `write` and argues that prompt injection is contained because delete needs another scope. But `create_item` maps to a **PUT that overwrites** (design "MCP server": "the brief's CREATE: put an item"). The store keeps no history: M3 has no compaction, but reads return only the newest version. So an injected "set every item's value to ''" destroys data exactly as well as a delete, using only the default key.
- ADR-0027 says the damage is "bounded by last-write-wins". That's wrong: an agent's write is the newest write, so LWW makes it **win**. LWW bounds nothing here.
- **The confirmation is up to the client, not the server.** "The destructive hint makes the client ask a person" is stated as a control (ADR-0027 §5, the spec "Authorisation" and "Verified with a real client"). Under the MCP spec, tool annotations are untrusted hints. As far as I know, Claude Code asks for confirmation because of its permission settings, not because of `destructiveHint`, and a user can allow a tool permanently. With the delete-scoped key used in the demo, nothing on the server side asks a person.
- **The keys are too broad for an agent.** Keys are global across tables, with no per-table scope and no rate limit. There is also no audit field: logs exclude keys, which is right, but the design never says the key's **name** or principal is logged. So after an incident you can't answer "what did the agent change?".

**Why it matters**
The brief (M4 §3) asks what an agent "may and may not do" and how prompt injection is contained. The current answer overstates the protection. For operations, an agent stuck in a retry or overwrite loop can also burn the availability and latency SLOs with nothing to throttle it.

**What I'd do instead**
- Make `create_item` a real **create**: `If-None-Match: *`, so it fails if the key exists. That matches the brief's CREATE literally.
- If overwriting is needed, add a separate `update_item` that **requires** `If-Match` with the version the agent read. The conditional-write machinery is already planned, so this is cheap.
- Add table-scoped keys (e.g. `read:<table>`) and a per-key rate limit at the coordinator.
- Log `principal=<key name>` on every request and add it as a label on `kv_requests_total`. There are only a handful of keys, so the cardinality is fine.
- Map `X-KV-Superseded`, 412 and 400 into explicit tool results, so the agent knows its write didn't apply.
- Restate the injection analysis honestly: the default key can damage data by overwriting unless create-only semantics are enforced.

---

##### W5. The unauthenticated internal API is reachable from the one internet-facing host

**What's wrong**
- ADR-0027 and the api-keys spec leave `/internal`, `/health` and `/metrics` without keys, "protected only by the network".
- ADR-0029 then says "the coordinator's and the shards' groups admit the monitor group **on their ports**". The architecture diagram shows the replicas' `/metrics` on 8081/8082, the same ports that serve `GET /internal/v1/log` (ADR-0023), and that endpoint returns every record with its value.
- The monitor sits in a public subnet, runs four third-party images pulled from Docker Hub, and exposes Grafana on port 3000 to `AllowedClientCidr` over plain HTTP, with an admin password.
- So compromising the monitor (a Grafana vulnerability, a sniffed password, a malicious image) gives a full data dump through `/internal/v1/log`, **bypassing the API keys M4 just introduced**. Depending on what the coordinator's internal routes accept (I didn't read their contract), forged heartbeats could also feed false `(epoch, LSN)` values into promotion decisions (ADR-0020 §2–3).

**Why it matters**
M4 adds authentication specifically to bound what callers can do. This hole cancels that for the most exposed machine. It also widens how far an incident can spread at the exact point where M5 will give this host more power, as the autoscaler's input.

**What I'd do instead**
- Serve `/metrics` on a separate listener (e.g. :9101/:9102) and open only that port to the monitor's security group.
- Keep 8081/8082 open only between the coordinator and the replica groups.
- Put Grafana behind SSM port forwarding, as ADR-0029 already does for Prometheus and Alertmanager, with no public ingress, or at least TLS.
- Optionally, give the internal API a shared secret from Secrets Manager.

---

#### 2. Decisions where a rejected alternative is better

1. **ADR-0025: `prometheus/client_golang` (option 2) over hand-written exposition (option 1).** The ADR's only argument for option 1 is ADR-0009's preference for the standard library. Option 2 gives:
   - tested histograms and correct exposition: cumulative buckets, `+Inf`, label escaping, concurrency;
   - Go runtime and process collectors for free (they address W1's missing signals);
   - **exemplars**, which attach a `request_id` to histogram observations so Grafana can jump from a p99 spike straight to the slow request's Loki lines. That is the cheapest available stand-in for the missing tracing (Non-Goals).

   The "explainable without notes" criterion is still met, because the author writes every metric definition. I'd write a short ADR that supersedes ADR-0009 for this one dependency. If the lab forbids that, at minimum export `runtime/metrics` and emit the OpenMetrics exemplar syntax by hand.

2. **ADR-0028: for the M4 problem (the late retry), the runner-up approach — server-issued versions plus `If-Match` — is better than client timestamps.** The late retry is fundamentally an idempotency problem. A server-issued version returned on the 504, or an idempotency key deduplicated at the leader, solves it with no clock-skew failure modes. The HLC is still justified for M5 migration, so the best answer combines them: keep the HLC, assigned **only by the leader** and returned to the client; drop free-form client stamps. That is part of the runner-up, not all of it, but it beats the chosen design (see W2).

3. **ADR-0029: SSM-only access, already chosen for Prometheus and Alertmanager, should also apply to Grafana** (see W5). Not a listed alternative, but the ADR applies its own better pattern inconsistently.

4. **ADR-0029: Promtail.** Not a rejected alternative but an unconsidered one. To my knowledge Grafana deprecated Promtail in favour of **Grafana Alloy**, with support ending in early 2026, before this design's date. Please check. If that's right, the ADR should pick Alloy, or at least record that it deliberately chose an end-of-life agent.

The other rejected alternatives hold up: Datadog or CloudWatch (sandbox limits), vector clocks (one leader per shard), HTTP MCP transport, enforcement only in the MCP server, and not exposing delete (the brief requires it).

---

#### 3. Missing or weak compared with what the brief asks for M4

**Monitoring (§1)**
- Per-shard latency, throughput and error rate are missing (W1). This is the clearest gap against the brief's text.
- The alerts use "replica down", but the brief says "node down". With placement B (ADR-0020), one node takes out two replicas, so one node stop fires two `ReplicaDown` alerts and no node-level alert. Add a node-level rule (from the probe or `node_exporter`), or group by node in Alertmanager.
- `ErrorBudgetBurn` ("5xx share > 0.5% over 1 h") fires only once the hourly budget is **already spent**. It is a breach alarm, not a burn-rate alert. Add a fast window (e.g. 5xx > 5% over 5 min **and** over 1 min) that pages while there is still budget left.
- The latency SLO is evaluated over all shards together, which can hide one shard breaching it. Evaluate it per shard (that needs W1's label).
- ADR-0026's availability arithmetic (≈ 0.14% per leader change) counts only the writes to the failed shard. With placement B, a node failure also removes one replica of **both** shards. Unless the coordinator retries a failed replica read on the third replica (the documents I read don't say), about 2/3 of reads to both shards fail for up to 15 s until the replica is marked down. That roughly doubles the cost of a node failure and halves the "three per hour" headroom. Task 8.2 should count the 5xx responses during the leader-node drill and correct ADR-0026.
- The logs reach only coordinator → leader. The follower's side of a write and the commit wait can be joined cheaply: the leader logs `request_id ↔ LSN`, and followers log the LSN ranges they apply. The coordinator should also log how long each replica took on R = 2 reads. Accepting/emitting W3C `traceparent` instead of a custom `X-Request-ID` would make later OpenTelemetry adoption free.
- The "Lag visible" scenario measures lag on the follower itself. Lag computed on the leader (each follower's durable LSN and the time it was acked, which ADR-0023 §2 already tracks) is authoritative, and it stays visible when the follower is wedged or partitioned.
- Data is lost at teardown (ADR-0029, "Accepted downsides"). The brief's control question on incident investigation, and M5's before/after evidence, both need saved data. Snapshot the TSDB and Loki to S3 before teardown.

**Conflict resolution (§2)**
- The design meets the letter of the brief (timestamps, LWW, `If-Match` as the alternative strategy), but see W2: by default, LWW never rejects anything.

**MCP (§3)**
- CREATE is implemented as an upsert, not a create (W4).
- The error mapping doesn't cover superseded, 412, 400 or 401.
- Authorisation relies on the client's confirmation prompt; per-table scopes and auditing are missing (W4).

**Operational gaps not tied to one brief bullet**
- **Rotating a key requires restarting the coordinator** (ADR-0027, "Accepted downsides"). The coordinator is a single point of failure, so every rotation is a full outage that counts against the 99.5% SLO. Re-read the secret on SIGHUP or on a timer.
- `tasks.md` has no coordinator-down drill, no `absent()` rule tests, and no `amtool check-config` / `promtool check config` step.
- Task 9.1's runbook should cover what to do when the monitor itself is down.
- **OpenSpec housekeeping:** `kv-api` (M2) and `replication` (M3) are changed through new capabilities "since the M2 and M3 changes aren't archived yet" (`proposal.md`). After archiving, the specs will contradict each other (e.g. a PUT always writes vs. a PUT may be superseded; no auth vs. a key required). Plan follow-up MODIFIED deltas once M2 and M3 are archived.

## Verdicts

> Drafted by Claude Code on 2026-11-27 under the author's standing instruction to carry
> the work through to the lab session; **confirmed by the author** in chat on 2026-11-27
> ("я все приймаю"), together with the revised ADRs 0025–0029.

Points for the defense (accept or refute 2–3 with arguments): **accept** W2 (LWW by
default rejected nothing) and W5 (the monitor could read every value through the internal
API); **refute** `client_golang` and per-key rate limits in M4.

| Point | Verdict | What changes, or why not |
| --- | --- | --- |
| W1: no shard label on the coordinator's histogram | Accept | `kv_request_duration_seconds{op,shard}` |
| W1: replicas have no request latency or error metrics | Accept | `kv_replica_request_duration_seconds{op}` and `kv_replica_errors_total{op}` on every replica; this also shows which replica of an R = 2 read is slow |
| W1: no runtime metrics | Accept | Go runtime metrics (`runtime/metrics`: goroutines, GC pauses, heap) written by hand |
| W1: no host metrics (`node_exporter`) | Refute for M4 | The brief asks for request metrics, not host metrics; CPU and CPU credits come from EC2's own CloudWatch metrics, which we can read from the laptop (as in M3). Host metrics belong to M5's noisy-neighbour work |
| W1: M5 will depend on the monitor; data lost at teardown | Accept | ADR-0029 says the monitor's Prometheus is M5's scaling signal and stops being optional there; before teardown the evidence is exported from the laptop through SSM port forwarding (instances can't write to S3, M1 evidence 63) |
| W2: by default LWW rejects nothing (server stamps are always newest) | Accept | The leader returns its stamp on **every** write answer, the 504 included; a retry sends that stamp back. Stated honestly in the design: without a stamp a write is "last arrival wins" |
| W2: the 5 s bound can ratchet | Accept | Bound measured against the leader's **physical** clock |
| W2: "superseded" by a write that may never commit | Accept | A write is answered superseded only after the superseding record has committed; if that times out, 504 |
| W2: two version notions; `(epoch, LSN)` ETags break in M5 | Accept | ETag and `If-Match` use the HLC stamp (unique within a shard, because the leader's stamps rise strictly) |
| W2: no tie-breaker; migration can make new writes look old | Accept as M5 work | ADR-0028 notes that M5's migration adds the shard ID as a tie-breaker and applies the receive rule on the target |
| W3: a dead coordinator makes the SLI and replica alerts vanish | Accept | A probe from the monitor (`blackbox_exporter`, authenticated read through the coordinator every 5 s); `CoordinatorDown` critical on the probe or on `up`; `absent()` rules |
| W3: the monitor can't watch itself | Accept, in part | An always-firing `Watchdog` alert repeated hourly to Telegram: an hour of silence means the monitor is down. No external dead-man service (none available) |
| W3: detection time assumes unstated intervals | Accept | `evaluation_interval: 5s`, `group_wait: 5s` and the other Alertmanager timings pinned in the design; `ReplicaDown` without `for`; the time is reported as measured in task 8.2 |
| W3: noisy alerts at low traffic | Accept | Latency alerts need ≥ 1 request/s; `WritesNotCommitted` on a 504 rate, not a single one |
| W4: `write` can destroy data by overwriting | Accept | `create_item` is a real create (`If-None-Match: *`, 412 if the key exists); a separate `update_item` requires `If-Match` with the version the agent read; ADR-0027's injection analysis corrected ("LWW bounds nothing") |
| W4: the destructive hint is advisory | Accept | Stated as advisory; the server-side control is the scope. The demo uses Claude Code's permission prompt |
| W4: no table-scoped keys | Refute for M4 | One table in the demo; scopes per operation already separate read, write and delete. Table scopes are a small addition if M5's ops agent needs them |
| W4: no per-key rate limit | Refute for M4 | That is the course's Bonus 1 (API throttling, ADV-1), done on its own with its algorithm choice |
| W4: no audit of who did what | Accept | Every log line carries `principal` (the key's name); `kv_requests_total` gets a `principal` label |
| W4: tool results miss superseded, 412, 400, 401 | Accept | Mapped to explicit tool results |
| W5: the monitor reaches the internal API on the replicas' ports | Accept | `/metrics` on its own listener (coordinator 9180, replicas 9181/9182); the monitor's group admits only those ports |
| W5: Grafana open to the internet | Accept | No public ingress; Grafana through SSM port forwarding like Prometheus |
| W5: internal API without a secret | Refute for M4 | After the two changes above only the coordinator and the replicas reach the internal ports, inside two security groups; a shared secret adds rotation work for no new boundary. Kept as a known limit |
| §2: `prometheus/client_golang` is better | Refute | ADR-0009 (standard library), and every line stays explainable at the defense. Its two real gains are covered otherwise: runtime metrics by hand, and per-replica histograms with request IDs in logs instead of exemplars. Tests check our histograms against hand-computed buckets |
| §2: server-issued versions beat client clocks | Accept | Same change as W2 |
| §2: Grafana through SSM | Accept | Same change as W5 |
| §2: Promtail is end-of-life | Accept | Verified: Promtail reached end of life on 2026-03-02 (Grafana's Loki documentation); **Grafana Alloy** replaces it |
| §3: no node-level alert | Accept | `kv_replica_up` gets a `node` label; `NodeDown` when every replica on a node is down |
| §3: the error-budget alert is a breach alarm | Accept | Multi-window burn rate: 5xx above 5% over 5 min and over 1 min (fast); the hourly rule stays as the breach alarm |
| §3: latency SLO over all shards hides one shard | Accept | Evaluated per shard |
| §3: a node failure makes 2/3 of reads fail | Refute, in part | `coordinator.read` replaces a replica that doesn't answer with the next live one, so reads don't fail; but a stopped instance doesn't refuse connections, so those reads wait out the 3 s client timeout. Task 8.2 counts 5xx and measures that latency; ADR-0026 is corrected if the drill shows otherwise |
| §3: the follower's side of a write isn't in the logs | Accept | The leader logs request ID ↔ LSN; followers log the LSN ranges they append |
| §3: use W3C `traceparent` | Refute for M4 | One header we control is enough for log search; OpenTelemetry is outside M4. Noted for M5 |
| §3: lag measured on the follower | Accept | The leader exports each follower's acked LSN and seconds since its last ack; that is the lag alert's source |
| §3: key rotation restarts the coordinator | Accept | The coordinator reloads the keys file when it changes; `make rotate-keys` refreshes it through SSM |
| §3: tasks lack a coordinator-down drill, `absent()` tests, config checks | Accept | Added to tasks |
| §3: runbook for the monitor itself | Accept | Added to task 9.1 |
| §3: OpenSpec specs will contradict after archiving M2 and M3 | Accept | A task to write MODIFIED deltas once M2 and M3 are archived |
