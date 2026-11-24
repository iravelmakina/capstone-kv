# M2 benchmark: the baseline (ADR-0016)

The numbers M3 (replication) and M5 (autoscaling) are compared against.
Raw summaries: `bench-results/30e184676f85/20261124T123956Z.txt` (git-ignored locally) and
`s3://velmakina-kv-artifacts-<ACCOUNT_ID>/bench-results/30e184676f85/20261124T123956Z.txt`.

## Setup

| | |
| --- | --- |
| Store | release `30e184676f85`: coordinator + 2 shards, each **t3.small** (2 vCPU, 2 GiB), `unlimited` CPU credits, gp3 root volume; shards in two AZs (`use1-az1`, `use1-az2`) |
| Client | t3.small in public subnet B (a third placement), same VPC, talking to the coordinator's private IP |
| Tool | go-ycsb v1.0.3 (Go port of YCSB, same core workloads) with our binding `bench/capstonekv` |
| Data | 100,000 records × 10 fields × 100 bytes (~1 KB each), loaded once, `insertorder=hashed` |
| Runs | 1,000,000 operations per run, 16 client threads, **3 runs per workload, median reported** |
| Writes | `writeallfields=true`: an update rewrites the whole record, which is what our `PUT` does (ADR-0010) |
| Durability | every put is `fsync`ed to the shard's log before the reply (ADR-0013) |
| Date | 2026-11-24, 12:03–12:40 UTC, us-east-1 |

## Results (median of 3 runs)

Latency in milliseconds, measured by the client per operation (includes both network hops:
client → coordinator → shard).

### Workload A — 50% read / 50% update, zipfian

| Operation | Throughput (ops/s) | Avg | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| READ | 902 | 2.37 | 2.16 | 3.52 | 6.24 |
| UPDATE | 902 | 15.26 | 14.66 | 28.03 | 32.59 |
| **Total** | **1,804** | 8.82 | 3.51 | 26.06 | 30.30 |

### Workload B — 95% read / 5% update, zipfian

| Operation | Throughput (ops/s) | Avg | p50 | p95 | p99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| READ | 6,547 | 2.19 | 1.88 | 3.88 | 5.45 |
| UPDATE | 344 | 4.53 | 4.02 | 7.24 | 10.22 |
| **Total** | **6,891** | 2.31 | 1.94 | 4.20 | 6.52 |

### Load (100,000 inserts, once)

940 ops/s, avg 16.9 ms, p99 32.6 ms.

### Spread between runs

| | Run 1 | Run 2 | Run 3 |
| --- | ---: | ---: | ---: |
| A total, ops/s | 1,804 | 1,805 | 1,802 |
| B total, ops/s | 6,762 | 6,891 | 6,947 |

Under 3% apart: the baseline is stable. **No failed requests** in any run (0 logged; no
`READ_ERROR`/`UPDATE_ERROR` rows).

### CPU during the benchmark (CloudWatch, 12:03–12:40)

| Instance | CPU avg / peak | CPU credit balance min | Surplus credits used |
| --- | --- | --- | --- |
| coordinator | 39% / 69% | **0** | 0.09 (< $0.001) |
| shard-1 | 15% / 28% | 15.2 | 0 |
| shard-2 | 16% / 31% | 14.9 | 0 |
| client | 30% / 56% | 8.4 | 0 |

## What the numbers say

1. **Writes are limited by one lock held across `fsync`, not by the CPU or the volume's
   capacity.** A shard holds its write lock while `fsync` waits for the EBS volume, so
   writes to one shard happen one at a time: each waits for the previous one's round trip
   to the disk, far below what the volume could do in parallel. In
   workload A, 902 updates/s over 2 shards is ~450 per shard per second, about 2.2 ms per
   `fsync`. With 8 of the 16 threads updating at once, each update waits behind the others:
   median 14.7 ms. In workload B only ~0.8 updates are in flight, so an update costs about
   one `fsync` plus two network hops: 4.0 ms. The shards' CPU stays near 15%.
2. **Workload A's throughput is the write ceiling.** Reads aren't the limit (in B the same
   cluster serves 6,500 reads/s); A is capped because half of its operations queue for the
   disk. Batching several writes into one `fsync` (group commit) would raise it per shard,
   and is the cheaper fix; more shards raise it linearly (M5).
3. **The coordinator is the next bottleneck** (control question). It handles every request
   twice (client side and shard side), used up its CPU credits and drew on surplus ones.
   With `standard` credits it would have been throttled halfway through. It is not
   stateless: it holds the table registry and the shard membership in memory, and shards
   heartbeat to its one address, so scaling it out first needs both in a shared place.
4. **Why p99 is far above the average** (asked in the 20.11 defenses). In A, the average
   update is 15 ms but p99 is 33 ms: a request that arrives behind a full queue of writes on
   its shard waits for all of them. Reads in A have a p99 3× their median for the same
   reason: a read blocked by a write holding the lock. Max values up to 250 ms are rare
   stalls (EBS, Go garbage collection, the coordinator's CPU); they show at p99.99, not p99.
5. **Laptop vs AWS.** Locally an update took ~5 ms and reads 0.7 ms: a laptop SSD
   acknowledges `fsync` faster than EBS, and there was no network between containers. The
   AWS numbers are the baseline.

## Notes

- **Workload choice:** A and B are YCSB's standard key-value workloads: A stresses the write
  path (session store), B is the common read-mostly case (profiles, caches). C (read-only)
  is a subset of B; E needs range scans, which M2 doesn't have (ADR-0012).
- **YCSB key-range quirk:** for `zipfian`, YCSB's key range has no "−1" at the top, so one
  never-loaded key is occasionally requested; a read of it gets 404 (`READ_ERROR`). Expected
  rate ≈ 1/(records + 1): seen twice in a 5,000-record local test, never in these runs.
- **Integrity:** a local run with `dataintegrity=true` (every read checked byte for byte)
  found no mismatches.
- **How results leave the client:** the instance role can't write to S3 (M1 evidence 63),
  so `bench.sh` prints each run's final summary into the SSM output and `make bench-aws`
  stores it. The first benchmark attempt failed exactly there; see `docs/m2/pipeline.md`.

## How to repeat

```bash
make up-app BENCH=true      # app stack plus the client
make bench-aws RUNS=3       # ~40 min; results in bench-results/<sha>/
make down-app
```

or the manual workflow **benchmark** on GitHub (`.github/workflows/benchmark.yml`).
