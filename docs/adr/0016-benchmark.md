# ADR-0016: Benchmark tool and workloads

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

The brief: benchmark the sharded store with YCSB and keep the numbers as the baseline for
M3 and M5. YCSB needs a client ("binding") for our API. Control question: which workload,
and why it's representative for a key-value store. In the 20.11 defenses students used
workloads A and B; the lecturer asked about throughput vs latency and why p99 is far above
the average and p95.

## Options — tool

| | Original YCSB (Java) | go-ycsb (Go port, PingCAP) |
| --- | --- | --- |
| Workloads A–F, same property files | yes | yes |
| Our binding | a Java class to write and explain | a Go type in the same codebase |
| Recognised as "YCSB" | the reference | a faithful port; must be stated |

## Options — workloads

| Workload | Mix | Fits a KV store because |
| --- | --- | --- |
| **A** | 50% read / 50% update, zipfian | write-heavy session store; stresses the write path and fsync |
| **B** | 95% read / 5% update, zipfian | read-mostly cache-like use, the common KV case |
| C | 100% read | a subset of B |
| E | scans | M2 has no range queries |

## Decision

**go-ycsb with our own binding, workloads A and B.**
- **Setup:** 3 × t3.small (coordinator + 2 shards), client t3.small in the same VPC;
  100,000 records of 10 × 100-byte fields (YCSB default); 1,000,000 operations;
  threads 16 (tuned once, then fixed); each workload **3 runs**, the median reported.
- **Recorded:** throughput (ops/s), average, p95, p99 latency per operation; the
  instances' `CPUCreditBalance`; the commit SHA.
- **Kept in:** `docs/m2/benchmark.md` (results table, configuration) and the raw output in S3.

**Runner-up:** original YCSB. Reconsider if the tutor requires the Java reference.

## Accepted downsides

- go-ycsb isn't the reference implementation; its numbers are comparable to ours over time, not to published YCSB results.
- 3 runs is a small sample; enough for a baseline, not for fine comparisons.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Lab session ends mid-run | Medium | One run takes minutes; results uploaded after each run |
| Noisy neighbours on t3 | Low | Median of 3; same instance types in M3 and M5 |

## Related

- Implemented in: `bench/` (binding and runner), `.github/workflows/benchmark.yml`.
- Evidence: `docs/m2/benchmark.md`.
