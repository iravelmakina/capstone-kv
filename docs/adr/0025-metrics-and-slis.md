# ADR-0025: Which metrics we collect, and which are SLIs

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("я все приймаю"),
  together with the AI-review verdicts. Drafted with Claude Code from the decision list she
  approved; revised after the AI design review (`docs/m4/ai-review.md`).
- Date: 2026-11-27

## Context

The brief asks for latency (p50, p95, p99), throughput and error rate for the coordinator
**and the shards**, and for a dashboard and alerts on cluster health, replication lag and
shard distribution. Today the coordinator's `/health` has 503/504 counts and each
replica's `/health` has role, epoch, last LSN, lag and commit-wait p99; nothing stores them
over time. CloudWatch custom metrics are denied in the sandbox, and our instances can't
send metrics outside the VPC without a NAT (ADR-0005), so metrics are pulled from inside
the VPC by Prometheus (ADR-0029). ADR-0009 prefers the standard library. In M5 the p99 that
triggers autoscaling has to come from these metrics, per replica set.

## Options: how the code exposes metrics

| | 1. Prometheus text format, written with the standard library | 2. `prometheus/client_golang` | 3. Keep `/health` JSON and convert it |
| --- | --- | --- | --- |
| New dependencies | none | the client and its dependencies (about 10 modules) | none |
| Histograms | fixed buckets, ~100 lines of our code | complete, tested | none: only what `/health` computes |
| Runtime metrics, exemplars | runtime by hand (`runtime/metrics`); no exemplars | built in | no |
| Explainable "without notes" | yes, our code | mostly | yes |
| Fits ADR-0009 | yes | no | yes |

## Decision

**Option 1.** A small `internal/metrics` package writes counters, gauges and fixed-bucket
histograms in the Prometheus text format on `/metrics`, served on a **separate port** from
the API (coordinator 9180, replicas 9181/9182), so the monitor never needs access to a
port that serves item data (ADR-0029).

**Collected:**

| Metric | Where | Why |
| --- | --- | --- |
| `kv_requests_total{op,code,shard,principal}` | coordinator | throughput and errors as the client sees them, and who made them |
| `kv_request_duration_seconds{op,shard}` (histogram) | coordinator | p50/p95/p99 per operation and per shard (M5 scales per replica set) |
| `kv_replica_up{shard,replica,node}`, `kv_shard_has_leader`, `kv_shard_leader_epoch` | coordinator | cluster health; a node's replicas together; promotions as epoch steps |
| `kv_replica_last_lsn` | coordinator | where each replica is, from heartbeats |
| `kv_replica_request_duration_seconds{op}` (histogram), `kv_replica_errors_total{op}` | every replica | the shards' own latency and errors; which replica of an R = 2 read is slow |
| `kv_replica_role`, `kv_store_keys`, `kv_log_bytes`, `kv_replica_reads_total` | every replica | shard distribution, growth, read balance |
| `kv_commit_wait_seconds` (histogram) | leader | the part of write latency spent waiting for a follower |
| `kv_follower_acked_lsn{follower}`, `kv_follower_lag_seconds{follower}` | leader | replication lag seen by the leader, which keeps counting when a follower is wedged or cut off |
| `go_goroutines`, `go_gc_pause_seconds`, `go_heap_bytes` | every process | runtime health |

**SLIs: availability and p99, nothing else.** Availability is the share of public requests
answered below 500, counted at the coordinator, together with the share of successful
probes made from the monitor (ADR-0029): the probe still counts when the coordinator itself
is down and its own counters vanish. Latency SLIs are read and write p99 per shard at the
coordinator. They are what a client experiences. The rest are causes: they explain an SLI
breach and feed alerts, but a client never sees lag or commit wait directly, and a replica
down with a quorum left costs the client nothing. Making them SLIs would page for things
that don't hurt anyone.

Histogram buckets (seconds): 0.001, 0.002, 0.004, 0.008, 0.015, 0.03, 0.06, 0.12, 0.25,
0.5, 1, 2, 5. They put bucket edges at both SLO thresholds (15 and 60 ms), so the SLO checks
don't depend on interpolation inside a bucket.

**Not collected:** host metrics (CPU, disk, steal) through `node_exporter`. The brief asks
for request metrics; CPU and CPU credits come from EC2's own CloudWatch metrics, which we
read from the laptop as in M3. Host metrics belong to M5's noisy-neighbour work.

**Runner-up:** option 2. Reconsider if our histograms turn out wrong in tests, or if M5
needs exemplars to jump from a p99 spike to a request.

## Accepted downsides

- p99 from fixed buckets is an estimate: accurate at the bucket edges, interpolated between.
- No exemplars: a slow request is found through per-replica histograms and request IDs in
  the logs, not by clicking a point on a graph.
- One more code path on every request; measured with a benchmark run (task 8.3).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Label sets grow and Prometheus memory with them | Low | Labels are fixed: operation, code, shard, replica, node, principal (a handful of keys) |
| Our histogram code has a bug | Medium | Unit tests against hand-computed buckets; values compared with go-ycsb's percentiles in the benchmark run |

## Related

- ADR-0026 (SLO values), ADR-0029 (how metrics are collected and shown), ADR-0027
  (principals).
