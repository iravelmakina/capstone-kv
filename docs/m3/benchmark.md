# M3 benchmark: replication against the M2 baseline

> **Draft**, written by Claude Code from the runs of 2026-11-25/26; the author reviews it
> and rewrites the explanations in her own words.

Same benchmark as M2 (ADR-0016), so the numbers compare directly. Raw summaries, git-ignored
locally and kept under the same paths in the artifacts bucket:

| Run | File (`bench-results/891923e65ef2/`) | Session |
| --- | --- | --- |
| W = 2, workloads A and B | `20261125T235803Z.txt` | 1 (2026-11-25 23:12–23:58 UTC) |
| W = 1, workload A | `20261126T014009Z-w1.txt` | 1 (2026-11-26 01:09–01:40 UTC), after a redeploy |
| W = 2, workload A, control | `20261126T215529Z-w2-control.txt` | 2 (2026-11-26 21:20–21:55 UTC) |
| W = 2, workload A, node-c stopped | `20261126T223149Z-follower-down.txt` | 2 (2026-11-26 21:56–22:31 UTC) |

Session 2 deployed the same release on fresh instances. Its control run repeats session 1's
W = 2 run of workload A, to show how much two deployments of the same thing differ
(`docs/m3/evidence/aws-deploy.txt` §A13–A20).

## Setup

| | M2 | M3 |
| --- | --- | --- |
| Release | `30e184676f85` | `891923e65ef2` |
| Store | coordinator + 2 shards, one instance each | coordinator + 3 nodes (one per AZ), each running one replica of each shard: 2 shards × 3 replicas (ADR-0020) |
| Quorums | — | **W = 2, R = 2** (default, ADR-0021); one run with **W = 1, R = 3** |
| Instances | t3.small, `unlimited` credits, gp3 | same |
| Client, data, runs | go-ycsb, 100,000 records × 1 KB, 1,000,000 operations, 16 threads, 3 runs, median of each figure | same |

## Results (median of 3 runs, latency in ms)

### Workload A — 50% read / 50% update

| | M2 | M3, W = 2 (session 1) | M3, W = 2 (session 2, control) | M3, W = 1 | M3, W = 2, one follower per shard stopped |
| --- | ---: | ---: | ---: | ---: | ---: |
| **Total, ops/s** | **1,804** | **1,542** | **1,545** | **1,728** | **1,533** |
| READ avg / p95 / p99 | 2.37 / 3.52 / 6.24 | 2.79 / 4.52 / 10.26 | 2.56 / 4.07 / 6.56 | 2.34 / 3.46 / 5.92 | 2.75 / 4.02 / 6.75 |
| UPDATE avg / p95 / p99 | 15.26 / 28.03 / 32.59 | 17.83 / 32.45 / 39.04 | 18.05 / 32.77 / 39.39 | 16.09 / 29.58 / 35.33 | 18.05 / 32.34 / 39.01 |

Against M2, W = 2: throughput −14%, update p99 +20–21% (both sessions agree); read p99 +64%
in session 1 but only +5% in session 2 (see "Why p99 changed", point 2).

### Workload B — 95% read / 5% update (session 1 only)

| | M2 | M3, W = 2 | change |
| --- | ---: | ---: | ---: |
| **Total, ops/s** | **6,891** | **5,272** | −24% |
| READ avg / p95 / p99 | 2.19 / 3.88 / 5.45 | 2.75 / 5.10 / 8.25 | p99 +51% |
| UPDATE avg / p95 / p99 | 4.53 / 7.24 / 10.22 | 8.17 / 12.33 / 16.59 | p99 +62% |

### Load (100,000 inserts)

M2 940 ops/s; M3 W = 2 507 (session 1) and 779 (session 2); W = 1 889; follower down 774.

### Spread between runs

| | Run 1 | Run 2 | Run 3 |
| --- | ---: | ---: | ---: |
| A, W = 2, session 1, ops/s | 1,457 | 1,542 | 1,547 |
| B, W = 2, session 1, ops/s | 5,280 | 5,210 | 5,272 |
| A, W = 1, ops/s | 1,728 | 1,727 | 1,730 |
| A, W = 2, session 2 control, ops/s | 1,542 | 1,550 | 1,545 |
| A, W = 2, follower down, ops/s | 1,523 | 1,533 | 1,535 |

The first A run of session 1 was about 6% slower than the other two, probably warm-up of
the freshly started instances; the rest are within 1–2%. **No failed requests** in any run.

## Why p99 changed

1. **A write now waits for a second disk and a network round trip.** With W = 2 the leader
   flushes the record, a follower's long-poll returns it (one hop), the follower flushes it
   and acks with its next pull (one hop back), and only then does the leader answer
   (ADR-0023). Workload B shows the cost best, because its updates rarely queue: the
   average update went from 4.5 to 8.2 ms, about one more fsync (~2 ms in M2) plus a
   cross-AZ round trip. In workload A updates queue behind each other on the leader as in
   M2, so the extra time is a smaller share: p99 +20%, the same in both sessions.
2. **A read now waits for the slower of two replicas, but by less than session 1
   suggested.** With R = 2 the coordinator asks two replicas and answers when both have,
   so its latency is the maximum of two and the tail grows more than the average. In
   session 1 read p99 in A rose from 6.2 to 10.3 ms; in session 2, the same release and
   settings on new instances, to 6.6 ms. Throughput and update latency matched within 1%,
   so the read tail is the figure most sensitive to the machines a deployment lands on
   (other tenants on the host, the state of new gp3 volumes). The honest summary: R = 2
   costs reads +5% to +64% at p99 depending on the deployment, and one session is not
   enough to say more. Workload B (session 1 only) showed +51%.
3. **W = 1 (asynchronous followers) gives back most of the write cost.** Throughput in A
   went from about 1,545 to 1,728 ops/s, within 4% of M2, and update p99 from 39 to 35 ms:
   the leader answers after its own fsync. The price is durability: a write acked with
   W = 1 is on one disk until a follower pulls it, and R = 3 makes every read wait for all
   three replicas (any replica down fails reads).
4. **The earlier open question is mostly answered.** Session 1 had W = 1 reads (R = 3,
   5.9 ms) faster at p99 than W = 2 reads (R = 2, 10.3 ms). Against session 2's control
   (6.6 ms) the gap is 0.6 ms, no longer a puzzle: most of it was the difference between
   deployments. The rest fits a replica's store holding its lock while it flushes, so
   reads queue behind log writes that take longer with W = 2; not verified (a profile of
   one replica under load, M5).
5. **One follower of each shard down costs almost nothing.** Throughput −0.8%, read p99
   +3%, update p99 unchanged against the control of the same session, and no 503 or 504.
   Each write still needs exactly one follower, and the remaining one is always it; each
   read pairs the two live replicas instead of rotating over three. The leader no longer
   gets "the faster of two followers", but with both in other AZs the two were alike.
   When node-c came back, its replicas pulled ~0.8 million missing records each and
   reached the leader in about 15 s, with no errors (evidence §A18).

## CPU credits

All instances run `unlimited`, so nothing was throttled. The nodes and the coordinator
started with almost no credits and stayed at 0 through both sessions, using surplus credits
instead: in session 2 about 22 in all (coordinator 16, node-a 3.7, node-b 1.5, node-c 0.9),
roughly $0.02 (evidence §A19). The coordinator accrues surplus fastest: it is still the
busiest instance.

## Noisy neighbours

Each node runs two replicas (one of each shard, ADR-0020), so a leader shares its t3.small
with a follower of the other shard. Shard 1's leader is on node a and shard 2's on node b,
so the two leaders don't compete for one machine. Neighbours outside our control (other
tenants of the same host) are the likely cause of the read-tail difference between the
sessions (point 2).
