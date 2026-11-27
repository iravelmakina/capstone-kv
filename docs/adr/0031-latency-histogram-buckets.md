# ADR-0031: Finer latency histogram buckets

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("Accept it"),
  option B. Drafted with Claude Code at her request ("Yes, draft it") from the M4
  benchmark in AWS.
- Date: 2026-11-27
- Supersedes in part ADR-0025: the list of histogram buckets. The metrics, the SLIs and
  the choice of fixed buckets written by our own code stay.

## Context

ADR-0025 put bucket edges at both SLO thresholds (15 and 60 ms), so the SLO checks are
exact: "p99 above 60 ms" means "more than 1% of requests above the 60 ms edge", with no
interpolation. Its accepted downside was that a p99 *between* two edges is interpolated.
The AWS benchmark (`docs/m4/evidence/aws-deploy.txt`, B' and D1') showed how much that
costs:

| | go-ycsb at the client | Prometheus at the coordinator | Buckets around it |
| --- | ---: | ---: | --- |
| Write p99, workload A, 3 runs | 38.5 ms | 54–56 ms | 30 ms → 60 ms: nothing between |
| Read p99, same runs | 9.6 ms | 7.7 ms | 4 ms → 8 ms → 15 ms |

Writes: 94.3% were at or below 30 ms and 99.87% at or below 60 ms (bucket counts over the
benchmark). `histogram_quantile` assumes the 5.6% in between are spread evenly over
30–60 ms, so it puts the 99th percentile at ~55 ms. The client measured 38.5 ms. The
dashboard thus shows writes close to their 60 ms SLO when they are 20 ms away. The alert
isn't affected: it checks the 60 ms edge.

Reads: the coordinator's 7.7 ms falls between 4 and 8 ms, so it could be off by up to
~4 ms. The 1.9 ms gap to the client is partly network and the client itself, and the
current buckets can't tell how much.

This matters more in M5. The brief makes p99 per replica set the autoscaling trigger.
A scale-out threshold will sit below the SLO (say reads at 10–12 ms, writes at 40–50 ms),
exactly where the current buckets interpolate the most.

## Options

| | A. Add 40 and 50 ms | **B. Add 6, 10, 20, 40 and 50 ms** | C. Prometheus native histograms | D. Keep them, and read p99 values from the benchmark client |
| --- | --- | --- | --- | --- |
| Buckets | 15 | **18** | dynamic | 13 |
| Writes: widest bucket around 38 ms | 10 ms | **10 ms** | about 10% of the value | 30 ms |
| Reads: widest bucket around 8–10 ms | 7 ms (8–15) | **2 ms** | about 10% of the value | 7 ms |
| SLO edges (15, 60 ms) kept exact | yes | yes | no fixed edges: the SLO checks interpolate too | yes |
| Work | one line, tests | **one line, tests** | protobuf exposition: our text-format code (ADR-0025) can't write native histograms; needs `client_golang` or our own protobuf | none |
| Series added (coordinator: ~10 op × shard pairs) | +20 | **+50** | — | 0 |

**Option B.** `LatencyBuckets` becomes 0.001, 0.002, 0.004, 0.006, 0.008,
0.01, 0.015, 0.02, 0.03, 0.04, 0.05, 0.06, 0.12, 0.25, 0.5, 1, 2, 5 seconds. That's
18 edges, at most 2 ms apart up to 10 ms, 5 ms up to 20 ms and 10 ms up to 60 ms. Both SLO
edges stay. It covers the read range as well as the write range, because M5 will set
thresholds in both, and the cost is a few dozen series more than the few hundred we
already export.

**Runner-up: A**, if M5 ends up scaling on writes only.

## If accepted

- Code (a separate commit): `internal/metrics/metrics.go`, the bucket list and its
  comment. `internal/metrics/metrics_test.go` already checks that the list ascends and
  keeps both SLO edges; its hand-computed histogram is checked again. All three latency
  histograms use the list: the coordinator's requests, the replicas' requests, and the
  leader's commit wait (GC pauses have their own buckets). The recording rules, alerts
  and dashboard need no change:
  `histogram_quantile` uses whatever buckets the series have.
- A benchmark run in the next lab session compares the coordinator's p99 with the client's
  again. Writes should then come within the 30–40 ms bucket.
- ADR-0025 gets a "Superseded in part by ADR-0031" line; its text stays.
- Right after the deploy that brings the change, a rate window (5 minutes) can mix the old
  and the new buckets. Each instance restarts with the new release, so it lasts one window.

## Accepted downsides

- Still interpolated, only within narrower buckets: a p99 between 40 and 50 ms can be off
  by up to 10 ms.
- 18 buckets per series instead of 13: more lines on every scrape. Negligible at our size
  (a few hundred series of ours).

## Related

- ADR-0025 (metrics and buckets, superseded in part), ADR-0026 and ADR-0030 (the SLO edges
  kept), `docs/m4/evidence/aws-deploy.txt` (B', D1').
