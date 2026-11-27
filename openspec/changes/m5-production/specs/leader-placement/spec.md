# Spec Delta

## Purpose

Losing one node costs at most one shard's leader whenever the cluster can arrange it, so a
late node at start-up or a promotion doesn't leave several leaders on one machine, and a
leader can be moved on purpose without losing a write.

## ADDED Requirements

### Requirement: Leaders per node visible
The coordinator SHALL export the number of shard leaders per node, a node being the address
its replicas heartbeat from, and an info-level alert SHALL fire when a node holds more than
one leader while another node of the same sets holds none.

#### Scenario: Colocated leaders reported
- **WHEN** shard-1's and shard-2's leaders are both on node b and node a holds none, for 2 minutes
- **THEN** `LeadersColocated` fires and reaches the alert chat

### Requirement: Bounded wait at first assignment
The coordinator SHALL choose a replica set's first leader once all its expected replicas
are live, or after 3 minutes with a majority live, whichever comes first.

#### Scenario: Late node at start-up
- **WHEN** node a registers two minutes after nodes b and c at the first start
- **THEN** the first leaders are chosen after node a has registered, and no node holds two leaders

### Requirement: Placement tie-break
When choosing a first leader or promoting a follower, the coordinator SHALL pick among the
candidates with the highest `(epoch, LSN)` only (ADR-0020's safety rule); among those, it
SHALL prefer one on a node that holds no other leader.

#### Scenario: Equally caught-up followers
- **WHEN** shard-1's leader dies and both followers report the same `(epoch, LSN)`, one of them on the node that leads shard-2
- **THEN** the coordinator promotes the follower on the other node

#### Scenario: Safety first
- **WHEN** the only follower with the highest `(epoch, LSN)` is on a node that already leads another shard
- **THEN** the coordinator promotes it anyway

### Requirement: Planned leader transfer
The coordinator SHALL support moving a shard's leadership to a named, live follower through
an admin call: hold new writes to that shard; have the leader step down (it refuses writes
and reports its final committed LSN); wait until the follower has acknowledged that LSN;
persist the new epoch and push it to every replica of the set; then release the held writes
to the new leader. No acknowledged write SHALL be lost and no write SHALL get a 5xx because
of a transfer.

#### Scenario: Epoch pushed, not waited for
- **WHEN** a transfer completes
- **THEN** every live replica of the set reports the new epoch and leader before the first released write is sent

### Requirement: Transfer time bound
A transfer SHALL hold writes at most 2 s; if it can't finish within that, the old leader
SHALL resume in its epoch and receive the held writes, and the epoch SHALL NOT change.

#### Scenario: Target too slow
- **WHEN** the named follower hasn't acknowledged the leader's final LSN within 2 s
- **THEN** the old leader resumes, the epoch is unchanged, and the held writes are served by it

#### Scenario: Transfer under load
- **WHEN** leadership of shard-1 moves from 1b to 1a under workload A
- **THEN** the epoch rises by one, writes resume on 1a within 2 s, and the run has no 5xx caused by the transfer

#### Scenario: Write in flight during step-down
- **WHEN** a write reaches 1b just as it is told to step down
- **THEN** it either commits before 1b reports its final LSN and is on 1a after the transfer, or is refused and retried on 1a; a later read returns the value the client was told was written

### Requirement: No automatic transfer of a healthy leader
The coordinator SHALL NOT move a live leader on its own; a transfer SHALL happen only when
requested through the admin API (an approved ops-agent proposal or a person).

#### Scenario: Colocated leaders left alone
- **WHEN** both leaders are on node b and every replica is live
- **THEN** no transfer starts until one is requested and approved
