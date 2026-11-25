# Proposal

> **Approved** by the author on 2026-11-25. Drafted with Claude Code from the decision list
> agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).

## Why

In M2 each shard is a single instance: lose its disk and its keys are gone, and while it's
down its keys return 503. M3 makes every shard a replica set of three, so data survives the
loss of a replica, reads spread over all replicas, and the store keeps answering with one
replica down. The YCSB numbers are compared with the M2 baseline.

## What Changes

- **Replica sets:** each shard becomes 3 replicas, one per AZ, with one leader. Three shard
  instances, each running one replica of each shard.
- **Replication log:** records carry a format version, the leader epoch, an LSN, a hash
  of the previous record and a checksum over everything; deletes leave tombstones. A key's
  version is `(epoch, LSN)`.
- **Sync:** followers pull the leader's log from their last record and verify it; a
  returning replica cuts back what the leader doesn't have.
- **Writes:** go to the leader only and commit when the leader and one follower have them
  on disk (W = 2, a setting); the leader shows a write to readers only after it commits.
  A commit timeout returns **504 "outcome unknown"**.
- **Reads:** the coordinator asks 2 of the 3 replicas (R = 2), rotating, and returns the
  newer version.
- **Leader loss:** the coordinator promotes the most up-to-date follower automatically once
  both followers answer, under a new epoch that it keeps on disk.
- **Metrics:** each replica reports role, epoch, last LSN, lag and commit-wait time.
- **Infrastructure:** the app stack goes from 2 shard instances to 3 (5 instances in all).
- **Benchmark:** the same YCSB workloads A and B, compared with M2, plus a W = 1 run and a
  run with a follower stopped.
- **ADRs:** 0020 replication model, placement and leader changes, 0021 N/W/R, 0022 log
  structure and ordering, 0023 replica sync and rejoin.

## Capabilities

### New Capabilities

- `replication`: replica sets and placement, the log format, sync, rejoin, write commit, leader changes, metrics.
- `quorum-reads`: R = 2 reads over rotating replicas, newest version wins, tombstones.
- `replication-benchmark`: YCSB on the replicated store, compared with the M2 baseline.

### Modified Capabilities

- `sharding` (M2): a shard ID now names a replica set; membership and availability are per replica.
- `shard-storage` (M2): new record format; deletes leave tombstones.
- `kv-api` (M2): a write can return 504.
- `app-deployment` (M2): three shard instances in three AZs, two containers each.

These are written as new requirements in the capabilities above; the M2 change isn't
archived yet, so its specs can't be modified as deltas.

## Non-Goals

- Leaderless replication, conflict resolution, client request IDs, a clock per record (M4).
- Group commit and hedged reads (M5, as p99 work measured on its own).
- Merkle trees: the hash chain covers divergence detection.
- Log compaction; a replicated coordinator.

## Impact

- Code: `internal/store`, `internal/shard`, `internal/coordinator`, `api/openapi.yaml`
  (504), `api/internal-openapi.yaml`, `docker-compose.yml`.
- AWS: `velmakina-kv-app` with 5 instances while it's up (≈ $0.10/h), deployed only for
  smoke tests and the benchmark, then torn down. $36.6 of the credit is left after M2.
