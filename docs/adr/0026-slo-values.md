# ADR-0026: SLO values

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("я все приймаю"),
  together with the AI-review verdicts. Drafted with Claude Code from the decision list she
  approved; revised after the AI design review (`docs/m4/ai-review.md`).
- Date: 2026-11-27

## Context

The SLIs are availability and read and write p99 at the coordinator (ADR-0025). The brief
asks for an SLO and where its numbers came from. What we have measured (M3, workload A,
`docs/m3/benchmark.md`): read p99 6.6–10.3 ms depending on the deployment, write p99 about
39 ms; with one follower per shard down, no change and no errors. By design, a leader's
loss gives 15–20 s of 503 on writes to its shard (ADR-0020), and the store runs only during
lab sessions of a few hours, so a 30-day window means nothing here.

## Options

| | 1. Tight (from the best run) | 2. Measured + headroom | 3. Loose |
| --- | --- | --- | --- |
| Availability | 99.9% | **99.5% per hour** | 99% |
| Read p99 | 8 ms | **15 ms** | 50 ms |
| Write p99 | 40 ms | **60 ms** | 200 ms |
| A leader change in an hour of workload A (≈ 20 s of 503 on one shard's writes ≈ 0.14% of requests) | breaks it (budget 0.1%) | fits, about three per hour | fits easily |
| Breached by the deployment-to-deployment variance we saw | yes | no | no |

## Decision

**Option 2.**

- **Availability ≥ 99.5%** of public requests answered below 500 and of probes from the
  monitor, per hour of operation.
- **Read p99 ≤ 15 ms** and **write p99 ≤ 60 ms**, at the coordinator, over 5 minutes,
  **per shard**: one shard breaching its SLO isn't hidden by the other being fast.

Where the numbers come from:
- 15 ms is about 1.5× the worst read p99 we measured (10.3 ms) and 2× the usual (6.6 ms):
  it allows the variance between deployments without hiding a real regression.
- 60 ms is about 1.5× the measured write p99 (39 ms), which already includes the follower's
  fsync and a cross-AZ round trip; a second fsync stuck behind a slow disk crosses it.
- 99.5% per hour fits about three leader changes per hour in workload A (each ≈ 20 s of
  503 on one shard's writes, ≈ 0.14% of requests), but not a shard that is fully
  unavailable for more than ≈ 36 s (half of all requests failing).
- A stopped node also takes one replica of each shard. Reads don't fail for it (the
  coordinator replaces a replica that doesn't answer), but until the node is marked down
  they wait out the 3 s client timeout: a latency cost, not an availability one. The node
  drill (task 8.2) counts 5xx and measures that latency; this ADR is superseded if it
  shows otherwise.

The alerts follow from them (ADR-0029): a shard's p99 above its SLO for 5 minutes (only
at ≥ 1 request per second, so a few slow requests at idle don't page); a **fast burn**
(5xx above 5% over both 5 minutes and 1 minute: at that rate the hour's budget goes in
about 6 minutes) that pages while budget is left; and the hourly budget spent.

**Runner-up:** option 1 for reads only. Reconsider after M5's p99 work (hedged reads,
group commit), when the read tail should be tighter.

## Accepted downsides

- Measured under one load (go-ycsb, 16 threads); a heavier load could breach the latency
  SLO without anything being wrong. The SLO is for the benchmark's load level.
- An hourly window is short: a single bad minute is a large share of it.

## Related

- ADR-0025 (SLIs), ADR-0020 (leader change time), `docs/m3/benchmark.md`.
