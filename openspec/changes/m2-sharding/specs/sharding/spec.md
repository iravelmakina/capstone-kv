# Spec Delta

## Purpose

How the coordinator spreads keys over shards and keeps track of which shards exist.

## ADDED Requirements

### Requirement: Consistent hashing ring
The coordinator SHALL map each item to a shard with a consistent hashing ring using
xxHash64, with the number of virtual nodes per shard set in ADR-0011, positioned by a
stable shard ID.

#### Scenario: Even spread
- **WHEN** the distribution test hashes 1,000,000 keys onto a ring of 2, 3 and 5 shards
- **THEN** with the chosen virtual-node count, no shard deviates more than 10% from the mean, and the results are recorded in `docs/m2/ring-distribution.md`

#### Scenario: Few keys move when a shard joins
- **WHEN** a third shard is added to a ring of two in the test
- **THEN** about one third of the keys change shard (between 25% and 42%) and no key moves between the two original shards

#### Scenario: Restart doesn't move keys
- **WHEN** a shard restarts with the same ID at a different address
- **THEN** every key maps to the same shard as before

### Requirement: Routing by partition key
The coordinator SHALL choose the shard from the table name and the partition key only;
the sort key SHALL NOT affect the choice.

#### Scenario: Same partition, same shard
- **WHEN** two items share a table and partition key but differ in sort key
- **THEN** both are stored on the same shard

### Requirement: Shard registration and heartbeat
A shard SHALL register with the coordinator at start and repeat it every 5 seconds; the
coordinator SHALL add unknown shard IDs to the ring.

#### Scenario: Shard joins
- **WHEN** a shard starts and calls `POST /internal/v1/shards` with its ID and address
- **THEN** the coordinator lists it in `GET /internal/v1/shards` and routes keys to it

#### Scenario: Coordinator restart
- **WHEN** the coordinator restarts
- **THEN** it answers item requests with 503 until the expected number of shards has re-registered, then serves them

### Requirement: Unavailable shard
A shard that misses 3 heartbeats SHALL be marked unavailable but stay on the ring; only
an explicit call SHALL remove it.

#### Scenario: Shard down
- **WHEN** a shard stops sending heartbeats for 15 seconds
- **THEN** requests for keys it owns return 503, and requests for other shards' keys still succeed
