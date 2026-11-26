# Spec Delta

## Purpose

The cost of replication, measured against the M2 baseline.

## ADDED Requirements

### Requirement: Same benchmark as M2, plus two variants
The benchmark SHALL run go-ycsb with workloads A and B and the record count, operation
count and threads of ADR-0016, from the client in the same VPC, against the replicated
store with W = 2; and workload A twice more: with W = 1, and with W = 2 and one follower
of each shard stopped.

#### Scenario: Runs complete
- **WHEN** the benchmark workflow runs against the M3 stack
- **THEN** each configuration completes 3 runs with no failed operations

### Requirement: Comparison recorded
`docs/m3/benchmark.md` SHALL show M3 and M2 side by side (throughput, average, p95, p99
per operation, median of 3 runs), the W = 1 and follower-down runs, the configuration,
`CPUCreditBalance`, the commit SHA, and an explanation of why p99 changed.

#### Scenario: Comparison readable
- **WHEN** a reader opens `docs/m3/benchmark.md`
- **THEN** it shows the numbers for every configuration and M2, and says why p99 changed
