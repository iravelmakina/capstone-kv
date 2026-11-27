# Spec Delta

## Purpose

The final benchmark shows, with autoscaling on, how latency, throughput and capacity change
under gradual, sudden and sustained load, and how the result compares with the M2 baseline.

## ADDED Requirements

### Requirement: Load profiles
The final benchmark SHALL run YCSB workload A with autoscaling on, driven by a target
request rate with 64–128 client threads, each profile starting from 2 replica sets:
**gradual** (5-minute steps from the M4 rate to the high rate), **spike** (the high rate
for 15 minutes, then back) and **sustained** (the high rate for 30 minutes). The high rate
SHALL be at least twice the rate at which calibration first crossed a scale-out threshold.

#### Scenario: Spike recorded
- **WHEN** the spike profile runs
- **THEN** the results show the time from the spike to the scale-out decision, to the new set registering, and to the migration finishing

### Requirement: Comparison runs like M2's
Besides the profiles, unthrottled workloads A and B SHALL run with 16 client threads, as in
M2, on 2 and on 3 replica sets with autoscaling off.

#### Scenario: Scaling factor measured
- **WHEN** the comparison runs are done
- **THEN** `docs/m5/benchmark.md` has throughput and latency for A and B on 2 and 3 sets next to M2's numbers, and states how far throughput grew from 2 to 3 sets

### Requirement: Calibration before thresholds are trusted
Before the final runs, a calibration run SHALL step a target rate on 2 replica sets until
p99 crosses the scale-out thresholds, recording coordinator and leader p99 against request
rate with the coordinator's CPU and credits. If the coordinator saturates before the
leaders cross their thresholds, or the thresholds don't fall between normal load and
saturation, the change SHALL stop for a new ADR before the final runs.

#### Scenario: Knee found
- **WHEN** calibration ends
- **THEN** `docs/m5/benchmark.md` has a table of request rate against coordinator and leader p99 and coordinator CPU on 2 replica sets, and states the rate at which each threshold was crossed

### Requirement: What is recorded
For every profile, latency (p50, p95, p99 at the client and p99 per shard at the
coordinator), throughput, error count, the number of replica sets on the ring, live
replicas and nodes, write holds, and scaling decisions SHALL be recorded over time, exported
from the monitor before teardown, and compared with the M2 baseline (`docs/m2/benchmark.md`).

#### Scenario: Comparison with M2
- **WHEN** the final runs are done
- **THEN** `docs/m5/benchmark.md` has, for each profile, graphs or tables over time and a section stating where M5 is better and worse than M2, with the reasons

### Requirement: Correctness checked during scaling
A verifier SHALL write and read back keys during every join and leave in the lab and report
the number of acknowledged writes and how many read back with their acknowledged value.

#### Scenario: Join under load verified
- **WHEN** shard-3 joins during a profile
- **THEN** the verifier reports every acknowledged write read back with its value

### Requirement: Sessions planned to fit
The lab work SHALL be planned so that each profile, with the reset to 2 replica sets before
it, finishes within one lab session's credentials; the sustained profile SHALL run in a
session of its own.

#### Scenario: Reset between profiles
- **WHEN** one profile ends with 3 replica sets
- **THEN** shard-3 is removed and the cooldown has passed before the next profile starts, and both are in the evidence

### Requirement: Monitor keeps its data through a test session
The monitor SHALL keep metrics and logs for a full lab session of load tests (at least 4
hours at the final benchmark's highest rate) without filling its disk, and the lab check
SHALL report its free space before and after.

#### Scenario: Long session
- **WHEN** four hours of load tests have run
- **THEN** the monitor's data disk is less than 80% full, and Prometheus and Loki still answer queries for the whole session
