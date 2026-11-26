# M3 benchmark: replication against the M2 baseline

> **Draft**, written by Claude Code from the runs of 2026-11-25/26; the author reviews it
> and rewrites the explanations in her own words. One run is still missing: workload A with
> one follower per shard stopped (lost when a deploy from `main` replaced the stack,
> `docs/m3/evidence/aws-deploy.txt` §A8).

Same benchmark as M2 (ADR-0016), so the numbers compare directly. Raw summaries:
`bench-results/891923e65ef2/20261125T235803Z.txt` (W = 2) and
`bench-results/891923e65ef2/20261126T014009Z-w1.txt` (W = 1), git-ignored locally and kept
under the same paths in the artifacts bucket.

## Setup

| | M2 | M3 |
| --- | --- | --- |
| Release | `30e184676f85` | `891923e65ef2` |
| Store | coordinator + 2 shards, one instance each | coordinator + 3 nodes (one per AZ), each running one replica of each shard: 2 shards × 3 replicas (ADR-0020) |
| Quorums | — | **W = 2, R = 2** (default, ADR-0021); one run with **W = 1, R = 3** |
| Instances | t3.small, `unlimited` credits, gp3 | same |
| Client, data, runs | go-ycsb, 100,000 records × 1 KB, 1,000,000 operations, 16 threads, 3 runs, median | same |
| Date | 2026-11-24 | 2026-11-25 23:12–23:58 UTC (W = 2), 2026-11-26 01:09–01:40 UTC (W = 1) |

## Results (median of 3 runs, latency in ms)

### Workload A — 50% read / 50% update

| | M2 | M3, W = 2 | change | M3, W = 1 |
| --- | ---: | ---: | ---: | ---: |
| **Total, ops/s** | **1,804** | **1,542** | −15% | **1,728** |
| READ avg / p95 / p99 | 2.37 / 3.52 / 6.24 | 2.79 / 4.52 / 10.26 | p99 +64% | 2.34 / 3.46 / 5.92 |
| UPDATE avg / p95 / p99 | 15.26 / 28.03 / 32.59 | 17.83 / 32.45 / 39.04 | p99 +20% | 16.09 / 29.58 / 35.33 |

### Workload B — 95% read / 5% update

| | M2 | M3, W = 2 | change |
| --- | ---: | ---: | ---: |
| **Total, ops/s** | **6,891** | **5,272** | −24% |
| READ avg / p95 / p99 | 2.19 / 3.88 / 5.45 | 2.75 / 5.10 / 8.25 | p99 +51% |
| UPDATE avg / p95 / p99 | 4.53 / 7.24 / 10.22 | 8.17 / 12.33 / 16.59 | p99 +62% |

### Load (100,000 inserts)

M2 940 ops/s; M3 W = 2 507 ops/s; M3 W = 1 889 ops/s.

### Spread between runs

| | Run 1 | Run 2 | Run 3 |
| --- | ---: | ---: | ---: |
| A, W = 2, ops/s | 1,457 | 1,542 | 1,547 |
| B, W = 2, ops/s | 5,280 | 5,210 | 5,272 |
| A, W = 1, ops/s | 1,728 | 1,727 | 1,730 |

The first A run with W = 2 was about 6% slower than the other two, probably warm-up of the
freshly started instances; the rest are within 1–2%. **No failed requests** in any run.

## Why p99 changed

1. **A write now waits for a second disk and a network round trip.** With W = 2 the leader
   flushes the record, a follower's long-poll returns it (one hop), the follower flushes it
   and acks with its next pull (one hop back), and only then does the leader answer
   (ADR-0023). Workload B shows the cost best, because its updates rarely queue: the
   average update went from 4.5 to 8.2 ms, about one more fsync (~2 ms in M2) plus a
   cross-AZ round trip. In workload A updates queue behind each other on the leader as in
   M2, so the extra time is a smaller share: p99 +20%.
2. **A read now waits for the slower of two replicas.** With R = 2 the coordinator asks two
   replicas and answers when both have; its latency is the maximum of two, so the tail
   grows more than the average (B: average +26%, p99 +51%). Each read also costs the
   coordinator two requests instead of one, on an instance that already ran out of CPU
   credits in M2.
3. **W = 1 (asynchronous followers) gives back most of the write cost.** Throughput in A
   went from 1,542 to 1,728 ops/s, within 4% of M2, and update p99 from 39.0 to 35.3 ms:
   the leader answers after its own fsync. The price is durability: a write acked with
   W = 1 is on one disk until a follower pulls it, and R = 3 makes every read wait for all
   three replicas (any replica down fails reads).
4. **Open question:** reads with W = 1 (R = 3) had a *lower* p99 (5.9 ms) than with W = 2
   (R = 2, 10.3 ms), although they wait for three replicas instead of two. A likely cause is
   that a replica's store holds its lock while it flushes, so reads on a replica queue
   behind its log writes, and with W = 2 those writes take longer. Not verified yet; a
   profile of one replica under load would settle it (M5, with the p99 work).

## CPU credits

All instances run `unlimited`, so nothing was throttled. The new nodes started with almost
no credits and stayed near 0 through the W = 2 run (surplus credits used, see
`docs/m3/evidence/aws-deploy.txt` §A12); in the W = 1 run, started after the instances had
accrued some, balances grew. The coordinator's balance fell steadily in both runs: it is
still the busiest instance.

## Noisy neighbours

Each node runs two replicas (one of each shard, ADR-0020), so a leader shares its t3.small
with a follower of the other shard. Shard 1's leader is on node a and shard 2's on node b,
so the two leaders don't compete for one machine.

## Still to run

- Workload A with one follower of each shard stopped (node-c stopped), 3 runs.
