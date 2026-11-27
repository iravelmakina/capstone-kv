# Spec Delta

## Purpose

A replica set can join or leave the hash ring while the store serves requests: only the
keys whose owner changes move, reads never stop, no acknowledged write is lost, nothing
uncommitted comes back, and the shard sizes before and after are recorded.

## ADDED Requirements

### Requirement: Pending shards
A replica set that registers with the coordinator and is not on the ring SHALL be held as
*pending*: it gets a leader and heartbeats like any set, but no client request is routed to
it until a migration adds it to the ring.

#### Scenario: Registration doesn't remap keys
- **WHEN** shard-3's replicas register while shards 1 and 2 hold data
- **THEN** every existing key still reads from shard 1 or 2, and shard-3 is listed as pending in the cluster state

### Requirement: Ring changes only through a migration
Adding a pending set to the ring or removing a set from it SHALL happen only through a
migration started by an admin call (`admin` scope). At most one migration SHALL run at a
time. Each virtual node's position SHALL stay `xxhash64(shardID + "#" + i)` with 128
virtual nodes per shard (ADR-0011). Only sets added after the base sets SHALL be removable.

#### Scenario: Second migration refused
- **WHEN** an admin starts a migration while another is running
- **THEN** the call returns 409 and names the running migration

#### Scenario: Base set can't leave
- **WHEN** an admin asks to remove shard-1
- **THEN** the call returns 409 and says only extra sets can be removed

### Requirement: Only moving keys move
A migration SHALL copy exactly the keys whose owner differs between the ring before and the
ring after the change, including deleted keys (tombstones), each with its stamp.

#### Scenario: Adding a third shard
- **WHEN** shard-3 is added to a ring of shards 1 and 2 holding 100,000 keys
- **THEN** the migration copies only the keys the new ring assigns to shard-3 (about a third), and no key whose owner is unchanged is written anywhere

### Requirement: Old owner serves until its cutover
Until a source's cutover, every read and write of its moving keys SHALL be served by that
source. From its cutover, reads and writes of those keys SHALL be served by the new owner
only.

#### Scenario: Reads during migration
- **WHEN** a client reads a moving key while its copy is in progress
- **THEN** the source answers with the latest committed value, with no 5xx

### Requirement: Only committed records are copied
The copy SHALL take the source leader's commit point once before its first page, read the
current records of moving keys as of that point, and then follow the source's log from that
point, receiving only records the source has committed. A copied record SHALL be applied on
the target only if its stamp is newer than what the target holds for that key (ADR-0028),
and the target leader SHALL advance its clock past every stamp it receives.

#### Scenario: Write during copy
- **WHEN** a client updates a moving key after the copy of that key but before cutover
- **THEN** the update reaches the target once the source has committed it, and after cutover a read returns the updated value

#### Scenario: Uncommitted write on a dying source
- **WHEN** the source leader appends a write to a moving key and dies before any follower has it
- **THEN** the target never holds that write, and after cutover the key reads its last committed value

### Requirement: Freeze at the source
At a cutover, the source leader SHALL append a replicated freeze record for the moving keys
and refuse writes to them from that record on, with 409 "moved"; any later leader of that
shard whose log has the record SHALL do the same. A replicated unfreeze record SHALL lift
it.

#### Scenario: Freeze survives a leader change
- **WHEN** the source leader dies after its freeze record committed
- **THEN** the promoted leader also refuses writes to the moving keys with 409 "moved"

### Requirement: Cutover fenced at the source
For each source, the coordinator SHALL hold new writes to its moving keys, have the source
freeze them, wait until the target has applied the source's committed records through the
freeze record, switch the keys to the new owner, and send the held writes and any write
refused as "moved" to it. A held write SHALL wait at most 2 s; otherwise the freeze SHALL be
lifted and the held writes sent to the source. Writes to other keys and all reads SHALL
never be held.

#### Scenario: In-flight write at cutover
- **WHEN** a write to a moving key was sent to the source just before the hold started and commits during the freeze
- **THEN** it is either before the freeze record and reaches the target, or refused as "moved" and applied on the target; in both cases the client's acknowledged value is what a later read returns

#### Scenario: Short write pause
- **WHEN** a cutover happens under 800 requests per second
- **THEN** no request gets a 5xx because of it, no write to a moving key waits more than 2 s, and reads and writes to other keys show no added wait

#### Scenario: Target too slow
- **WHEN** the target can't catch up within 2 s
- **THEN** the freeze is lifted, the held writes are served by the source, the migration stays in its copy phase, and the cutover is retried

### Requirement: Purge after cutover
After every source has cut over, each source shard SHALL remove the moved keys from its
memory through replicated purge records of at most 1,000 keys each, so its key count and
the keys it serves drop. A purged key SHALL NOT become a tombstone. Freeze and purge records
SHALL use a new record format version, which a binary that doesn't know it refuses at
start.

#### Scenario: Old shard shrinks
- **WHEN** a migration adding shard-3 is done
- **THEN** the old shards' `kv_store_keys` drop by the number of keys moved from each, and the sum over all shards equals the number before the migration

### Requirement: Removal
Removing a set SHALL move all its keys to the owners in the ring without it, by the same
copy, cutover and purge steps, and then mark the set retired so it can be stopped.

#### Scenario: Shard-3 removed
- **WHEN** shard-3 is removed after holding writes of its own
- **THEN** every key it held reads from shard 1 or 2 with its latest value, and shard-3 is reported retired

### Requirement: Migration survives a coordinator restart
The coordinator SHALL persist the migration's phase and ring change, and on restart SHALL
resume it from the last completed phase; a migration SHALL also be abortable before any
cutover, leaving the old ring in place and purging what was copied.

#### Scenario: Restart mid-copy
- **WHEN** the coordinator restarts during a copy
- **THEN** it comes back with the same migration in its copy phase, continues it, and no key reads as missing at any point

### Requirement: Copy rate budget
The copy SHALL be throttled to a configurable rate (records per second), set so that it
adds no more than a stated budget (from calibration) to the source leader's write p99.

#### Scenario: Throttled copy
- **WHEN** a migration runs at its default rate under the calibration load
- **THEN** the source leader's write p99 during the copy exceeds its value just before by no more than the budget

### Requirement: Progress, alerts and evidence
The coordinator SHALL export the migration's phase, records copied and records left per
source; an info-level alert SHALL fire when a migration makes no progress for 5 minutes.
For every join and leave, the keys, log bytes and request rate per shard before and after
SHALL be recorded from the dashboard, with a verifier's result.

#### Scenario: Before/after recorded
- **WHEN** shard-3 has joined during a benchmark
- **THEN** `docs/m5/rebalancing.md` has the keys per shard before and after, with the spread (max/min) within ADR-0011's 10% of the mean, and the verifier's count of acknowledged writes read back

#### Scenario: Stalled migration
- **WHEN** the target stops pulling for 5 minutes during a copy
- **THEN** `MigrationStalled` fires and reaches the alert chat
