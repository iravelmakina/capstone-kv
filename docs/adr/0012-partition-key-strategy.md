# ADR-0012: Partition key strategy

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

A key decides which shard holds a record. The lecturer's first rule (20.11): choose it by
the **access pattern** — a query that has to scan or join across shards defeats sharding,
however even the spread is. Then: high cardinality, even spread, and the key never changes
after the record is written. The brief offers an optional compound key (partition key +
sort key).

## Options

| | 1. Hash the whole key (pk + sk) | 2. Hash only table + partition key | 3. Range partitioning on the key |
| --- | --- | --- | --- |
| All items of one partition on one shard | no | **yes** | yes |
| Future "all items for pk, sorted by sk" | scans every shard | one shard | one shard |
| Even spread | best | good if pk has high cardinality | hot ranges (e.g. timestamps) |
| Rebalancing | consistent hashing | consistent hashing | split ranges |

## Decision

**Option 2.** Shard = ring(`xxhash64(table + "\x00" + partitionKey)`). The sort key is not
hashed: it only distinguishes items inside a partition, so all items of one partition stay
on one shard (DynamoDB does the same). Tables declare their key names at registration
(ADR-0010); a table without a sort key uses the partition key alone.

**Access pattern this serves:** point reads and writes by full key, which is everything the
brief and YCSB issue. No M2 operation touches more than one shard.

**Runner-up:** option 1. Reconsider if a table turns out to have a few huge partitions.

## Accepted downsides

- A partition with very many items, or very hot, lands on one shard (the "hot partition" problem).
- Range queries by sort key are designed for, not built in M2.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Low-cardinality partition key chosen by a user (e.g. a country) | Medium | Documented guidance; YCSB keys are high-cardinality |

## Related

- Implemented in: `internal/coordinator/` (key → shard), `internal/ring/`.
- Uses: ADR-0011 (hash, ring).
