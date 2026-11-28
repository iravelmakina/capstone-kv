# M5 control questions — short answers

> **Drafted with Claude Code** as defense prep (task 9.2). The author answers these orally
> at the defense; this file is notes, not the author's own text. Numbers marked _local_ come
> from the Docker cluster on the laptop (`docs/m5/rebalancing.md`,
> `docs/m5/autoscaling-local.md`); the AWS numbers come from the lab sessions (group 8) and
> are _to be filled_ until then.

## 1. What metrics are best for autoscaling stateful vs stateless services?

- **Stateless** (a web tier behind a load balancer): any instance can take any request, so
  a new one helps at once. Utilisation (CPU, requests per instance, queue length) works
  because it predicts latency and capacity arrives in a minute.
- **Stateful** (our replica sets): a new set helps only after its data has moved in, and a
  set's latency depends on where its keys are, its replication and its disk, not on CPU. In
  M3 the write ceiling was the leader's fsync and the W = 2 round trip, with the CPU far
  from full. So we scale on what the client feels: **p99 latency per shard**, the brief's
  rule, plus a **cause check** on the shard's own metric (ADR-0032): write p99 > 50 ms at
  the coordinator *and* the leader's own write p99 > 40 ms for 3 min; reads > 12 ms and the
  slowest replica > 8 ms for 5 min. The cause check stops us adding a set when the
  coordinator is the bottleneck (`CoordinatorBound` alert instead).
- **Guards** belong to stateful scaling: no scale action while a replica is down, a set has
  no leader, a write hold happened in the last 5 min, within 10 min of the last migration,
  or below 100 req/s (p99 of a handful of requests is noise).

## 2. Explain the challenges of rebalancing data in a distributed system. How do you minimise impact?

- **Challenges:** copying while clients keep writing (the copy goes stale); the moment of
  switching ownership (a write accepted by the old owner after the copy finished is lost);
  failures mid-way (a restart must resume, not restart or double-apply); the load the copy
  itself puts on the sources; deletes (a key deleted during the copy must not come back);
  and knowing when it is done.
- **What we do (ADR-0033):** the new set stays `pending` (on no ring) until it serves. Each
  source sends a snapshot as of its commit point, then its committed tail filtered to the
  moving keys, then a **per-source cutover**: the coordinator holds writes to that source's
  moving keys only (≤ 2 s), the source writes a replicated **freeze record** (later writes
  to those keys get 409 "moved"), the tail up to the freeze is applied, the ring switches,
  writes resume. Copies apply newer-only by HLC, so a retry or a resumed copy is harmless;
  tombstones are copied too. Progress is persisted, so the coordinator resumes after a
  restart. A rate limit (2,000 records/s) bounds the copy's load. Only after every source
  cut over do the sources purge the moved keys (purge records, replicated).
- **Measured** _(local)_: join in 10 s, leave in 10 s; 1,564 writes during the join, 0
  failed, 0 lost. AWS: _to be filled_ (task 8.x).

## 3. How does consistent hashing minimise data movement during rebalancing?

- With `hash(key) mod n`, going from 2 to 3 sets moves ≈ 2/3 of keys. On a ring, a new set
  takes over only the arcs its virtual nodes land on: ≈ 1/(n+1) of the keys, here ≈ 1/3,
  and only from the sets whose arcs it splits. Every other key stays put.
- 128 virtual nodes per set (ADR-0011) spread those arcs, so the new set takes a little
  from every set and the result is even: _local_ after the join 15,278 / 16,727 / 18,136
  keys, all within 10% of the mean (16,714).
- The migration moves only the keys whose owner changed: each source filters its snapshot
  and tail with the before/after rings (`movingFilter`), not a full copy.

## 4. What's the difference between horizontal and vertical autoscaling? When to use each?

- **Horizontal:** more instances (here: a third replica set). Raises the ceiling roughly
  linearly when work splits by key, survives single instance loss, but needs data movement
  and routing for state. Our case: writes are bounded per leader (fsync + quorum), so more
  leaders is what raises the write ceiling.
- **Vertical:** a bigger instance. No data movement, but a restart (downtime per node), a
  hard upper limit, and it doesn't help a bottleneck that is not CPU/RAM (fsync latency,
  network round trip). We used it once on purpose: the coordinator went to t3.medium
  (ADR-0035) because it is a single routing instance that every request crosses, and
  `CoordinatorBound` tells when the coordinator, not a shard, is the limit.

## 5. What are the risks of aggressive scale-down policies?

- **Flapping:** scale in, latency rises, scale out, each move a migration with a write hold
  and copying load. **Losing capacity just before the next peak.** **Scaling in during a
  failure**, when the remaining sets have less redundancy. **Moving data twice** for nothing.
- **Ours (ADR-0032):** scale in only after 10 min below the thresholds (`ScaleInLatencyOk`),
  10 min after the last migration, never with a replica down; **and** the request rate must
  be below `0.75 × (n−1)/n` of the rate at which the set was added. That last check is
  beyond the brief's "below the threshold for N minutes": with three sets, latency is low
  *because* of the third set, so latency alone would remove it under the same load and
  flap. Only the extra set (shard-3) can leave; the two base sets never do.
- **Measured** _(local)_: see `docs/m5/autoscaling-local.md`.

## 6. How do you ensure zero downtime during shard addition/removal?

- Reads never stop: the old owner serves until its cutover, the new owner after it, and the
  coordinator routes by the ring that is current at that moment.
- Writes pause only for the moving keys of one source at a time, at most 2 s, and are
  answered after it (the coordinator waits for the hold and retries once on 409 "moved"),
  so the client sees latency, not errors. If a cutover misses its 2 s deadline it is undone
  (unfreeze) and retried later.
- A set is added to the ring only once it serves (confirmed live leader) and removed only
  after its keys moved out and sources took them over (`leaving` → `retired`), so no key is
  ever without an owner. Its instances stop after it is retired.
- **Measured** _(local)_: 0 errors in the verifier during both migrations; AWS: _to be filled_.

## 7. What actions does your ops agent propose, and why does execution sit behind human approval?

- **Kinds (ADR-0034):** `transfer_leader` (two leaders share a node, so one node failure
  stops two shards' writes), `abort_migration` (stalled, and only before any cutover),
  `scale_up` / `scale_down` (only when the autoscaler is off or a guard blocks it), and
  `no_action`. Each comes with reasoning, evidence (tool values with times), expected effect
  and risks of acting and not acting.
- **Why a person approves:** the model reads alert text and metric labels, which anyone who
  can write to the store can influence (evaluation case 6: an alert that "orders" a
  scale-down). It can be wrong with confidence, and the actions move data. So it is L1: it
  only proposes, from a container without AWS credentials or the admin key, with a key that
  can only read cluster state. Deterministic checks (`ops.Check`) reject a proposal outside
  its bounds; the person sees the full proposal and types `yes`; it expires after 10 min;
  kvscale checks its guards again before executing. The judgement is measured by the
  evaluation set (`make ops-eval`): pass rate _to be filled_.

## 8. Compare your final benchmark against the M2 baseline. Where did you win, and where did you lose?

- **M2 baseline** (2 shards, no replication, workload A): 1,804 ops/s, update p99 32.6 ms,
  read p99 6.2 ms. **M3** (3 replicas, W = 2): 1,542 ops/s (−14%), update p99 39 ms (+20%):
  the price of durability across zones.
- **M5:** _to be filled from the AWS benchmark with autoscaling (task 8.x)_: gradual
  increase, spike, sustained load; throughput, p99 and active nodes over time. Expected:
  the ceiling rises with the third set (a third leader for writes), the price is the
  migration's short hold and copy load while it joins, and time to scale (launch + boot +
  ≥ 3 min of breach + the copy) during which a spike is served by two sets.
