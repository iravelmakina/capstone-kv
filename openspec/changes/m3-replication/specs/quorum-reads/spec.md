# Spec Delta

## Purpose

Reads see the latest committed write and spread over all replicas.

## ADDED Requirements

### Requirement: Quorum reads
For every read and exists, the coordinator SHALL ask R live replicas of the key's shard
(R = 2 with W = 2, R = 3 with W = 1) and return the answer with the higher version
`(epoch, LSN)`, where a tombstone counts as "not found".

#### Scenario: Newest version wins
- **WHEN** one replica answers a value at (3, 40) and the other a tombstone at (3, 90)
- **THEN** the read returns 404

#### Scenario: Newer epoch wins
- **WHEN** one replica answers a value at (2, 120) and the other a value at (3, 101)
- **THEN** the read returns the value at (3, 101)

#### Scenario: Read after write
- **WHEN** a put has been answered with success
- **THEN** every later read of that key returns the new value, even with any one replica stopped

### Requirement: Reads spread over all replicas
The coordinator SHALL choose the replicas for each read by rotating through the shard's
live replicas.

#### Scenario: Balanced reads
- **WHEN** 3,000 reads hit one shard with all 3 replicas live and R = 2
- **THEN** each replica serves between 1,800 and 2,200 of the 6,000 replica reads

### Requirement: Fewer than R replicas
A read SHALL return 503 when fewer than R replicas of its shard are live.

#### Scenario: Two replicas down
- **WHEN** two replicas of a shard are stopped and R = 2
- **THEN** reads of that shard's keys return 503
