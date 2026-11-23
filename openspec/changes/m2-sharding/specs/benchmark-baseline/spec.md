# Spec Delta

## Purpose

A recorded, repeatable YCSB baseline that M3 and M5 compare against.

## ADDED Requirements

### Requirement: Workloads and setup
The benchmark SHALL run go-ycsb with our binding against the deployed store, workloads A
and B, with the record count, operation count and threads from ADR-0016, from a client in
the same VPC.

#### Scenario: Runs complete
- **WHEN** the benchmark workflow runs
- **THEN** each workload completes 3 runs with no failed operations

### Requirement: Results kept
The results SHALL be recorded in `docs/m2/benchmark.md`: throughput, average, p95 and p99
latency per operation (median of 3 runs), the configuration, the instances'
`CPUCreditBalance` and the commit SHA; raw output SHALL be kept in S3.

#### Scenario: Baseline readable
- **WHEN** a reader opens `docs/m2/benchmark.md`
- **THEN** it shows the table for both workloads and enough configuration to repeat the run
