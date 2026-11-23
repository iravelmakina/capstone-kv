# ADR-0011: Hash function and number of virtual nodes

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

The coordinator maps each key to a shard with a consistent hashing ring, so adding or
removing a shard moves only the keys between neighbouring points. Each shard is placed on
the ring many times (virtual nodes). Two numbers decide how even the load is and what
happens when a shard leaves: the hash function and the virtual-node count. M2 runs 2
shards; M5 adds more.

## Options — hash function

| | xxHash64 | MurmurHash3 (64/128) | FNV-1a 64 | MD5 / SHA-1 |
| --- | --- | --- | --- | --- |
| Speed | fastest | fast | fast | slow (cryptographic) |
| Distribution | excellent | excellent | weaker on similar keys | excellent |
| In Go | `cespare/xxhash` (widely used) | third-party | standard library | standard library |
| Used by | many KV stores and caches | Cassandra | — | early Dynamo, Cassandra RandomPartitioner |

## Options — virtual nodes per shard

Few points → uneven arcs, and a leaving shard's keys fall on one neighbour. Many points →
even arcs and the leaving shard's keys spread over all others, at the cost of ring size
and lookup time (binary search over `shards × vnodes` points).

## Decision

**xxHash64**, and **128 virtual nodes per shard**, confirmed by measurement before the code
is final: hash 1,000,000 YCSB-style keys onto rings with 2, 3 and 5 shards at 1, 16, 64,
128 and 256 virtual nodes, and record the spread (max/min keys per shard, standard
deviation) in `docs/m2/ring-distribution.md`. **Acceptance:** with 128 points, no shard
deviates more than 10% from the mean. If 64 already meets it, 64 is used; if 128 doesn't,
256. A virtual node's position is `xxhash64(shardID + "#" + i)`, with `shardID` stable
across restarts, so a restart never moves keys.

**Runner-up:** MurmurHash3. Same quality, slightly slower; reconsider if a library issue appears.

## Accepted downsides

- One third-party dependency for the hash.
- A ring of 2 × 128 = 256 points is tiny today; at 5 shards it's 640, still trivial.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Uneven load despite an even key spread (hot keys) | Medium | Out of scope for the ring; partition key choice (ADR-0012) |

## Related

- Implemented in: `internal/ring/`, test `internal/ring/distribution_test.go`.
- Evidence: `docs/m2/ring-distribution.md`.
