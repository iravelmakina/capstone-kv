# Design

> **Approved** by the author on 2026-11-25. Drafted with Claude Code from the decision list
> agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).

## Context

- M2: a coordinator routes `table + partition key` through an xxHash64 ring (128 virtual
  nodes) to one of 2 shards; each shard keeps an in-memory map and an append-only log with
  checksums and fsync (ADRs 0011–0013, 0018). Shards register and heartbeat every 5 s.
- The network has three private subnets in `use1-az1/2/4` (ADR-0004).
- The lab denies MSK, MQ, SQS and Kinesis; $36.6 of the $60 credit is left.

## Goals / Non-Goals

**Goals:** 3 replicas per shard; reads and writes keep working with any one replica down;
no committed write is lost when a leader is replaced; a restarted or returning replica
catches up from where it agrees with the leader; a measured comparison with M2.

**Non-Goals:** leaderless replication, conflict resolution, group commit, hedged reads,
Merkle trees, log compaction, a replicated coordinator.

## Decisions

1. **Single leader per shard; 3 instances, one per AZ, each with one replica of each shard;
   automatic promotion by the coordinator under a persisted epoch.** ADR-0020.
2. **N = 3, W = 2, R = 2 by default; W is a setting (W = 1 with R = 3); 504 on a commit timeout.** ADR-0021.
3. **Leader's log with format version, epoch, LSN, hash chain and full checksum; version = `(epoch, LSN)`; tombstones.** ADR-0022.
4. **Pull with long-polling; the next pull is the ack; leader applies after commit; truncation on rejoin.** ADR-0023.
5. **Public API:** unchanged except a new 504 on writes.
6. **Benchmark:** go-ycsb, workloads A and B, same settings as M2 (ADR-0016), plus workload
   A with W = 1 and workload A with one follower stopped.

## Write path

```
client ──PUT──▶ coordinator ── ring → shard 1 ── its leader (epoch 3) ──▶ replica 1a
                                                  1. (epoch 3, LSN 101), hash link
                                                  2. append + fsync
                                                  3. wait for a follower's next pull
                                                     at ≥ 101 (≤ 2 s, else 504)
                                                  4. apply to its map → 200
replica 1b, 1c ──GET /internal/v1/log?epoch=3&lsn=100&hash=…──▶ replica 1a   (long-poll)
```

## Read path

```
client ──GET──▶ coordinator ── ring → shard 1 ── pick 2 live replicas (rotating)
                                     ├─▶ 1b: value v at (3, 40)
                                     └─▶ 1c: tombstone at (3, 90)
                               newest = (3, 90) → 404
```

## Placement

```
node a (use1-az1)          node b (use1-az2)          node c (use1-az4)
  1a  leader of shard 1      1b                         1c
  2a                         2b  leader of shard 2      2c
```

Coordinator (public subnet) and benchmark client: 5 instances in all.

## Membership and leader changes

Each replica sends `{shard, replica, address, epoch, lastLSN}` every 5 s; the answer
names the shard's current leader and epoch. The coordinator keeps leader and epoch per
shard in its log. If a leader misses 3 heartbeats, it asks both followers for their last
`(epoch, LSN)` and promotes the higher one under epoch + 1; if either doesn't answer, it
waits. At start it serves a shard once the leader is known and 2 of 3 replicas are live.

| Replicas live | Writes | Reads (R = 2) |
| --- | --- | --- |
| 3 | ok | ok |
| leader + 1 follower | ok | ok |
| 2 followers, leader down | 503 for ≈ 15–20 s, then ok on the promoted leader | ok |
| 1 | 503 | 503 |

## Failure modes, staleness and CAP

| Failure | What happens | Inconsistency, and how long | C or A |
| --- | --- | --- | --- |
| A follower stops | Writes commit on the other follower; reads pair the live two | None for reads (R + W > N); the stopped one is stale until it catches up | both |
| A follower lags | Its reads may be old, but every read pairs it with a replica holding the latest committed write | None returned to clients; lag shown as a metric | both |
| A commit times out | 504; the record is on the leader and may replicate | The write appears later or never; bounded by the next pull (≤ 7 s) or by truncation on a leader change | — |
| The leader stops | Writes 503 until promotion; reads continue | None: the promoted follower has every committed write | **CP** for writes |
| Coordinator cut off from a leader | The coordinator treats it as dead and promotes; the old leader gets no writes (only the coordinator sends them) and answers 409 once it hears the new epoch | None | **CP** |
| One AZ cut off | Each shard loses 1 replica; if a leader was there, it's replaced | None | both |
| Two replicas of a shard down | Writes and reads 503 | None | **CP**: refuses rather than answer stale |
| The coordinator stops | Everything stops (one instance, a known limit) | — | — |

**Staleness bound:** a client never reads older than the latest committed write while 2
replicas of the shard are live. A single follower's own lag is visible on its health
endpoint and is not exposed to clients.

## Timeouts

| What | Value |
| --- | --- |
| Heartbeat | 5 s |
| Unavailable / leader dead | 15 s (3 missed) |
| Long-poll held by the leader | 5 s |
| Follower's deadline on a pull | 7 s |
| Commit wait | 2 s → 504 |
| Coordinator asks a follower before promotion | 1 s |

## Local and AWS

- `docker-compose.yml`: coordinator + 3 nodes × 2 replicas, for development and every
  failure test above.
- AWS: `velmakina-kv-app` with 3 shard instances (t3.small, `unlimited` credits), the
  coordinator and the client: ≈ $0.10 an hour plus any CPU-credit surplus during the
  benchmark. Deployed for the smoke test and the benchmark only.

## Risks / Trade-offs

- [Write p99 rises: follower fsync + long-poll return] → expected; measured with W = 2 and W = 1.
- [Read p99 rises: slower of 2 replicas] → expected; measured, also with a follower stopped.
- [Two replicas per t3.small] → noisy neighbours noted with the benchmark.
- [Logs never compact] → fine at benchmark scale.
- [Budget] → Docker first, short AWS sessions, `make down-app` after each.
