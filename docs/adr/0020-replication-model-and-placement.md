# ADR-0020: Replication model, placement and leader changes

- Status: **Accepted** — 2026-11-25 by Iryna Velmakina (author). Drafted with Claude Code from
  the decision list agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).
- Date: 2026-11-25
- Supersedes: ADR-0014's placement "2 shards in two AZs", and ADR-0013's rule that a silent
  shard's keys return 503. The rest of both ADRs stands.

## Context

M3 adds durability: each shard becomes a set of 3+ replicas. The brief suggests a single
leader and leaves conflict resolution to M4. M1 built three private subnets in three AZs
(ADR-0004). The lab denies managed queues and brokers (MSK, MQ, SQS, Kinesis), its
instance cap is unknown, and $36.6 of the $60 credit is left after M2.

## Options — model

| | 1. Single leader per shard | 2. Leaderless (Dynamo-style) |
| --- | --- | --- |
| Writes go to | the shard's leader | any W replicas |
| Order of writes | one writer, so one order | concurrent writes, needs conflict resolution now |
| Brief | "suggested for simplicity" | "for the brave"; conflicts are M4's topic |

## Options — placement (2 shards × 3 replicas)

| | A. 6 instances, one replica each | B. 3 instances, one per AZ, each with one replica of each shard |
| --- | --- | --- |
| Instances with coordinator and client | 8 | 5 |
| Cost while up | ≈ $0.17/h | ≈ $0.10/h |
| Losing one instance | 1 replica of 1 shard | 1 replica of each shard; both still have 2 |
| Noisy neighbours | none | two replicas share a t3.small |

## Options — when the leader dies

| | No failover | Manual promotion | Automatic promotion by the coordinator | Raft-style election |
| --- | --- | --- | --- | --- |
| Who decides | nobody | a person | the coordinator (one authority, no votes) | the replicas, by vote |
| Safety rule enforced by | — | the operator | code | code |
| Effort | none | small | small + an epoch (needed anyway) | large |

## Decision

**Model: single leader per shard, N = 3.** Replica IDs are stable (`1a`, `1b`, `1c`,
`2a`, `2b`, `2c`).

**Placement: option B.** Three shard instances, one per AZ (`use1-az1`, `use1-az2`,
`use1-az4`), each running two containers: one replica of shard 1 and one of shard 2, on
different ports. Shard 1's leader starts on node a, shard 2's on node b, so writes don't
all land on one machine. With the coordinator and the benchmark client: 5 instances.

**Leader changes: automatic, by the coordinator, with a leader epoch.**
1. The coordinator keeps, per shard, the **leader and the epoch** (a counter, starting at 1)
   in its own log on disk, next to the table registry, so a restart doesn't forget them.
2. When the leader misses 3 heartbeats (15 s), the coordinator asks **both followers** for
   their last `(epoch, LSN)` directly. Only both together are sure to hold every committed
   write (W = 2, ADR-0021). If either doesn't answer, nothing is promoted and the shard's
   writes return 503 until the leader or the second follower is back.
3. It promotes the follower with the higher `(epoch, LSN)` (ties: the lower replica ID),
   increases the epoch, saves both, and announces them in its heartbeat answers.
4. A replica accepts writes and serves pulls only while it is the leader of the current
   epoch; otherwise it answers **409**. A returning old leader learns the new epoch from its
   first heartbeat, becomes a follower, and cuts its log back to what the new leader has
   (ADR-0023), so a write that never committed can't come back.

At start the coordinator serves a shard once it knows the shard's leader and at least 2 of
its 3 replicas are live, not all 3.

**For M5:** the scaling unit is a replica set (3 replicas in 3 AZs), not an instance.

**Runner-up:** leaderless. Reconsider in M4, where conflict resolution is the topic.

## Accepted downsides

- A node failure takes one replica of **both** shards at once (both keep 2 of 3).
- Two replicas share a t3.small: noisy neighbours, noted in the benchmark.
- Writes to a shard stop for ≈ 15–20 s while its leader is detected as dead and replaced.
- The coordinator is still one instance; if it's down, everything is (a known limit, outside M3).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A promotion loses a committed write | High | Promote only with both followers answering; the higher `(epoch, LSN)` has every committed write |
| Two replicas both act as leader | High | Only the coordinator assigns leaders, and it persists them; replicas obey the epoch and answer 409 when not the current leader |
| The lab caps instances below 5 | Low | 5 is a small number; the first deploy checks |

## Related

- Implemented in: `internal/coordinator/membership.go`, `internal/shard/`, `infra/app.yaml`.
- Uses: ADR-0021 (N/W/R), ADR-0022 (log), ADR-0023 (sync and rejoin).
