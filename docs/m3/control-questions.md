# M3 control questions — short answers

> **Drafted with Claude Code** as defense prep (task 9.1). The author answers these orally
> at the defense; this file is notes, not the author's own text. Each answer points to where
> this project shows it.

## 1. Benefits of replicating shards, and the drawbacks

**Benefits**
- **Durability:** a committed write is on two disks in two AZs (W = 2), so losing one
  machine, its disk or an AZ loses no committed data (ADR-0020, 0021).
- **Availability:** reads and writes keep working with any one replica of a shard down;
  in AWS, stopping a whole node (one follower of each shard) cost ~1% throughput and no
  errors (`docs/m3/benchmark.md`, point 5).
- **Read load spread:** the coordinator rotates the R = 2 replicas of each read over all 3,
  so each serves about 2/3 of its shard's reads (spec `quorum-reads`, "Balanced reads").

**Drawbacks**
- **Latency:** a write waits for a follower's fsync and a cross-AZ round trip; a read waits
  for the slower of two replicas. Update p99 +20%, throughput −14% in workload A.
- **Cost:** 3× the storage and instances per shard (3 nodes instead of 2 in our layout),
  and cross-AZ traffic on every write.
- **Complexity:** leader election, epochs, catch-up, truncation of diverged tails, 504
  "outcome unknown". Most of M3's code and failure tests are about these.

## 2. R/W quorum math, and why R + W > N gives consistent reads

- **N** replicas hold a key (3); a write commits after **W** have it on disk (2); a read
  asks **R** (2) and returns the newest version.
- If R + W > N, the set of replicas a read asks and the set that holds the last committed
  write **must share at least one replica** (pigeonhole: 2 + 2 = 4 > 3). That replica has
  the write, and its version `(epoch, LSN)` is the highest, so the read returns it.
- Ours: `R = N + 1 − W` in `cmd/kvnode/main.go`; W = 2, R = 2 by default, W = 1 forces
  R = 3. W = 1, R = 1 would break the rule and give stale reads (ADR-0021 table).
- The newest version must be comparable: epoch first, then LSN, so a write from a newer
  leader beats a higher LSN from an older one (spec "Newer epoch wins").

## 3. Leader-based vs leaderless replication

| | Leader-based (ours) | Leaderless (Dynamo, Cassandra) |
| --- | --- | --- |
| Who orders writes | one leader per shard: total order by LSN | nobody: concurrent writes to the same key conflict |
| Conflicts | none (one writer per epoch) | need versions + resolution (LWW, vector clocks, merges) |
| Leader failure | writes stop for 15–20 s until promotion | no failover pause: any W replicas accept |
| Repair | follower pulls the log; hash chain finds divergence | read repair, hinted handoff, Merkle anti-entropy |
| Prefer when | you need a clear order and simple reasoning (our KV, most databases) | write availability during failures and across regions matters more than order |

We chose leader-based (ADR-0020); leaderless is the runner-up, to reconsider in M4 where
conflict resolution is the topic.

## 4. The role of an append-only event log

- **The source of truth and the replication stream at once:** the leader appends a record
  `(epoch, LSN, op, key, value)`, fsyncs it, and the followers pull exactly those records
  (ADR-0022, 0023). The in-memory map is a cache rebuilt by replaying the log.
- **Order:** LSNs give one total order per shard; a follower applies in LSN order, so all
  replicas pass through the same states.
- **Cheap resync:** a restarted follower replays its own log and asks only for records
  after its last LSN (spec "Follower restarts behind"; in AWS node-c pulled ~0.8 million
  records in ~15 s, evidence §A18).
- **Integrity:** each record has a checksum and the hash of the previous one, so damage
  and divergence are detected, not silently served.
- Price: the log grows forever (no compaction in M3).

## 5. Write-ack strategies: sync vs async followers

| | W = 2 (one sync follower) | W = 1 (async followers) |
| --- | --- | --- |
| The leader answers after | its fsync + one follower's fsync and ack | its own fsync |
| Workload A throughput | 1,545 ops/s | 1,728 ops/s (+12%) |
| Update p99 | 39.4 ms | 35.3 ms |
| A write acked, then the leader's disk dies | survives on the follower | **lost** until a follower had pulled it |
| Reads | R = 2, survive one replica down | R = 3: any replica down fails reads |

Sync acks put the slowest of the needed replicas into the write's tail; async acks move the
risk into a window where an acked write lives on one disk. W = 2 is the default because
durability and reads surviving a lost replica are the point of M3 (ADR-0021).

## 6. Failure modes that lead to temporary inconsistency, and how staleness is bounded

From the design's failure-mode table (`openspec/changes/m3-replication/design.md`):
- **A follower stops or lags:** it holds old data, but every read pairs it with a replica
  that has the latest committed write (R + W > N), so clients don't see it; the lag is on
  its `/health`.
- **A commit times out (504):** the record is on the leader and may still replicate; the
  write appears later (≤ 7 s, next pull) or never (truncated if the leader is replaced).
  The client is told "outcome unknown", not "failed".
- **A leader is replaced:** a write it accepted but no follower had is dropped when it
  returns (truncation to where the logs agree).

**Bound:** a client never reads older than the latest committed write while 2 replicas of
the shard are live. Below that, reads return 503 instead of something stale.

## 7. When is the system CP, and when AP? A concrete scenario

- **CP for writes when a leader is lost:** node a (shard-1's leader) is cut off. For
  ≈ 15–20 s puts to shard-1 return 503, until the coordinator promotes the more up-to-date
  follower under epoch + 1. We give up availability rather than accept writes on two
  leaders. The old leader, if still running, gets no writes (only the coordinator sends
  them) and answers 409 once it hears the new epoch.
- **CP for reads with 2 replicas down:** stop node b and node c: shard-1 has only 1a live,
  fewer than R = 2, so its reads return 503 rather than possibly stale data.
- **"Both" with one follower down:** consistent and available at once, because a quorum is
  still there (the follower-down run: 0 errors).
- **Never AP in M3:** no mode serves possibly stale data or accepts writes without a
  quorum. The coordinator is a single point: if it stops, everything stops (a known limit).

## 8. How p99 changed after replication, and why exactly

From `docs/m3/benchmark.md` (workload A, median of 3 runs):
- **Update p99 +20% (32.6 → 39.4 ms):** the leader waits for a follower: the long-poll
  returns the record (one cross-AZ hop), the follower fsyncs (~2 ms) and acks with its next
  pull (one hop back). Workload B shows the per-write cost best: average update 4.5 → 8.2 ms.
- **Read p99 +5% to +64% depending on the deployment:** a read waits for the slower of two
  replicas, so the tail grows more than the average. The same release measured 10.3 ms in
  one session and 6.6 ms in another: the read tail is sensitive to the machines a
  deployment lands on.
- **W = 1 gives most of it back** (update p99 35.3 ms, throughput within 4% of M2): the
  extra fsync and round trip leave the write path.
- **One follower per shard down: no change** in update p99, read p99 +3%.
