# Spec Delta

## Purpose

The store adds a replica set when latency shows a shard is short of capacity, and removes
it when the load that caused it has gone, without a person deciding each step and without
scaling into a failure.

## ADDED Requirements

### Requirement: Scaling signal
Scaling decisions SHALL use only p99 latency (at the coordinator per shard, and each
shard's replicas' own latency as confirmation) and the coordinator's request rate, as
computed by the monitor's Prometheus. CPU, CPU credits and host metrics SHALL NOT trigger
scaling.

#### Scenario: CPU alone doesn't scale
- **WHEN** a node's CPU is at 100% while every shard's read and write p99 are below their scale-out thresholds
- **THEN** no scale-out is started

### Requirement: Scale-out condition for writes
A write scale-out SHALL be due when, for at least 3 minutes, a shard's write p99 at the
coordinator over 2 minutes is above 50 ms, its leader's own write p99 is above 40 ms, and
the shard receives at least 100 requests per second.

#### Scenario: Write latency in the shard
- **WHEN** shard-1's coordinator write p99 stays above 50 ms and its leader's write p99 above 40 ms for 3 minutes under 800 requests per second
- **THEN** the scale-out condition is firing, and the executor starts a scale-out if no guard blocks it

#### Scenario: Coordinator-bound
- **WHEN** the coordinator's write p99 is above 50 ms but every leader's write p99 is below 40 ms
- **THEN** no scale-out starts, and the executor reports the store as coordinator-bound

### Requirement: Scale-out condition for reads
A read scale-out SHALL be due when, for at least 5 minutes, a shard's read p99 at the
coordinator over 2 minutes is above 12 ms, at least one of its replicas' read p99 is above
8 ms, and the shard receives at least 100 requests per second. All scaling thresholds SHALL
be histogram bucket edges.

#### Scenario: Idle store doesn't scale
- **WHEN** a shard's read p99 is above 12 ms while it receives 20 requests per second
- **THEN** the scale-out condition does not fire

### Requirement: Scale-in condition
A scale-in SHALL be due only when every shard's read and write p99 at the coordinator over
5 minutes have stayed below the scale-out thresholds for 10 minutes and the total request
rate over those 10 minutes is below `0.75 × (n − 1) / n` of the rate recorded when the last
scale-out started, where n is the current number of replica sets.

#### Scenario: Latency recovered but load still high
- **WHEN** a third replica set brought p99 below the thresholds, and the request rate is still 90% of the rate at scale-out
- **THEN** no scale-in is started

#### Scenario: Load gone
- **WHEN** the request rate has been below 50% of the rate at scale-out and p99 below the thresholds for 10 minutes, with 3 replica sets
- **THEN** a scale-in of the extra replica set starts

### Requirement: Guards
The executor SHALL start at most one scaling action at a time and SHALL NOT start one while
any replica is reported down, a shard has no leader, a migration or leader transfer is in
progress, within 5 minutes after any write hold ended, or within 10 minutes after the
previous action finished. It SHALL keep between 2 and 3 replica sets and SHALL re-check
every guard immediately before acting.

#### Scenario: Replica down
- **WHEN** the scale-out condition fires while a node of shards 1 and 2 is stopped
- **THEN** no scale-out starts, and the executor logs that it was blocked by a replica down

#### Scenario: Upper bound
- **WHEN** the scale-out condition fires with 3 replica sets already on the ring
- **THEN** nothing is added, and the executor reports that the upper bound was reached

### Requirement: A damaged extra set can leave
A scale-in SHALL be allowed while a replica of the leaving set is down, if that set still
has a leader and 2 live replicas.

#### Scenario: Damaged extra set can still leave
- **WHEN** the scale-in condition holds and one shard-3 replica is down while shard-3 has a leader and 2 live replicas
- **THEN** the scale-in starts

### Requirement: Scaling state survives the executor
The rate at the last scale-out and the end time of the last action SHALL be stored by the
coordinator with the migration, and the executor SHALL read them from there.

#### Scenario: Executor restarted
- **WHEN** the executor restarts 3 minutes after a scale-out finished
- **THEN** it still applies the remaining 7 minutes of cooldown and the recorded scale-out rate

### Requirement: Scale-out sequence
A scale-out SHALL launch one replica set of three replicas, one in each AZ with its replica
ID fixed by its subnet, wait until the coordinator reports it registered with a leader and
at least 2 live replicas, then start the migration that adds it to the ring (see
`shard-rebalancing`). The action SHALL count as finished when that migration is done.
Launched instances SHALL NOT be terminated or replaced by AWS on its own initiative.

#### Scenario: New nodes take traffic
- **WHEN** a scale-out finishes
- **THEN** the coordinator's `kv_requests_total` shows requests for the new shard, and its replicas' `kv_store_keys` are above zero

### Requirement: Scale-in sequence
A scale-in SHALL first run the migration that removes the extra replica set from the ring,
and SHALL terminate its instances only after that migration is done and the coordinator
reports the set retired. A stack update SHALL NOT change the extra set's capacity, and a
deploy SHALL refuse to run while the extra set isn't retired.

#### Scenario: No data lost on scale-in
- **WHEN** a scale-in removes shard-3 while a verifier writes and reads keys
- **THEN** every write acknowledged before or during the scale-in reads back its latest value afterwards

#### Scenario: Deploy while scaled out
- **WHEN** someone runs the deploy while shard-3 is active
- **THEN** the deploy refuses and says shard-3 must be removed first

### Requirement: Decisions recorded
Every scaling decision, including a blocked one, SHALL be logged by the executor with the
time, the condition, the values that triggered it and the guard results, and SHALL be
delivered to the alert chat.

#### Scenario: Scale-out announced
- **WHEN** a scale-out starts
- **THEN** a message in the alert chat names the shard and value that triggered it, and the executor's log has the same line

### Requirement: Active capacity visible
The coordinator SHALL export the number of replica sets on the ring, the number of live
replicas and the number of nodes (instances) it knows, and the dashboard SHALL plot them
next to throughput and p99.

#### Scenario: Active nodes over time
- **WHEN** a scale-out finishes during a benchmark
- **THEN** the dashboard shows replica sets stepping from 2 to 3 and nodes from 3 to 6 at the times they happened
