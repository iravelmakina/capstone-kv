# Spec Delta

## Purpose

Every shard is a replica set of three that keeps its committed writes through the loss of
any one replica or a change of leader.

## ADDED Requirements

### Requirement: Replica sets and placement
Each shard SHALL be a replica set of 3 replicas with stable IDs and exactly one leader per
epoch. The replicas SHALL run on 3 instances in the 3 private subnets, each instance
holding one replica of each shard, with no public IP and inbound traffic only from the
coordinator's security group and their own.

#### Scenario: Replica sets registered
- **WHEN** all replicas have started and registered
- **THEN** `GET /internal/v1/shards` lists 2 shards, each with 3 live replicas, one leader and its epoch

#### Scenario: One replica of each shard per AZ
- **WHEN** the shard instances are listed
- **THEN** there is one in each of `use1-az1`, `use1-az2` and `use1-az4`, each running one replica of shard 1 and one of shard 2

### Requirement: Log records
Every log record SHALL carry a format version, the epoch, an LSN one higher than the
previous record's, the hash of the previous record, the operation, the key, the value, and
a checksum over all of it. A delete SHALL be recorded as a tombstone. A key's version
SHALL be the `(epoch, LSN)` of its last write.

#### Scenario: LSNs increase by one
- **WHEN** three writes are committed on one shard
- **THEN** their LSNs are consecutive and every replica's log holds them in that order

#### Scenario: Delete leaves a tombstone
- **WHEN** a key is deleted
- **THEN** every replica remembers it as deleted at the delete's version, also after a restart

### Requirement: Writes go to the current leader
The coordinator SHALL send every put and delete to the shard's leader. A replica that isn't
the leader of the current epoch SHALL answer writes and log pulls with 409.

#### Scenario: Write sent to a follower
- **WHEN** a put arrives at a follower
- **THEN** it answers 409 and its data doesn't change

### Requirement: Followers pull and verify the log
A follower SHALL ask the leader for the records after its last durable record, check each
record's checksum and its hash link to the one before, append and flush them, and apply
them in LSN order; records it already has SHALL be skipped.

#### Scenario: Follower stays current
- **WHEN** 1,000 puts are committed on a shard
- **THEN** within 5 seconds both followers' last LSN equals the leader's

#### Scenario: Broken hash link
- **WHEN** a pulled record's hash link doesn't match the follower's last record
- **THEN** the follower doesn't apply it and asks the leader where their logs agree

### Requirement: Resync after restart
A restarted follower SHALL replay its own log and then fetch only the records after its
last record.

#### Scenario: Follower restarts behind
- **WHEN** a follower is stopped, 500 puts are committed, and the follower is started again
- **THEN** it fetches records starting right after its last LSN, not from LSN 1, and catches up

### Requirement: Rejoin after a leader change
A replica whose last record isn't in the leader's log SHALL cut its log back to the last
LSN of its own epoch in the leader's log and continue from there.

#### Scenario: Former leader returns with an uncommitted tail
- **WHEN** a leader accepted a write that no follower received, was replaced, and starts again
- **THEN** it becomes a follower, drops that write, and ends up with the new leader's log

### Requirement: Rebuild of a damaged replica
A replica whose log is damaged in the middle SHALL be rebuildable from the leader by one
command that wipes its data and resyncs it.

#### Scenario: Rebuild
- **WHEN** `make rebuild-replica` is run for a replica whose log is damaged
- **THEN** the replica starts empty, pulls the leader's whole log, and its last LSN reaches the leader's

### Requirement: Write commit
With W = 2 the leader SHALL answer a write with success only after it and at least one
follower have the record on disk, and SHALL apply it to its own data only then. If no
follower has it within 2 seconds, the coordinator SHALL answer 504 "outcome unknown". With
W = 1 the leader's own flush SHALL be enough.

#### Scenario: One follower down
- **WHEN** one follower is stopped and a put is sent
- **THEN** the put succeeds

#### Scenario: Both followers down
- **WHEN** both followers are stopped and a put is sent
- **THEN** the put returns 504 within about 2 seconds, and a read from the leader doesn't return the new value

### Requirement: Automatic leader change
When a leader misses 3 heartbeats, the coordinator SHALL ask both followers for their last
`(epoch, LSN)`, promote the higher one under the next epoch, save leader and epoch in its
log, and announce them. If either follower doesn't answer, it SHALL not promote, and the
shard's writes SHALL return 503.

#### Scenario: Leader replaced
- **WHEN** a shard's leader stops for 15 seconds with both followers live
- **THEN** the more up-to-date follower becomes leader under a higher epoch, puts succeed again, and every put committed before is still readable

#### Scenario: Only one follower live
- **WHEN** a shard's leader and one follower are stopped
- **THEN** no follower is promoted and puts to that shard return 503

#### Scenario: Coordinator restart keeps the leader
- **WHEN** the coordinator restarts after a promotion
- **THEN** it still names the promoted replica as leader with the same epoch

### Requirement: Coordinator start
After a start, the coordinator SHALL serve a shard once its leader is known and at least
2 of its 3 replicas are live.

#### Scenario: One replica missing at start
- **WHEN** the coordinator starts while one replica of a shard is stopped
- **THEN** that shard's reads and writes succeed

### Requirement: Replication metrics
Each replica's health endpoint SHALL report role, epoch, last LSN, lag behind the leader
and commit-wait time; the coordinator's SHALL report 503 and 504 counts per shard.

#### Scenario: Lag visible
- **WHEN** a follower is stopped while puts continue, then started
- **THEN** its health endpoint shows a lag that falls back to 0
