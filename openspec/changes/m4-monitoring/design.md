# Design

> **Approved** by the author on 2026-11-27 ("я все приймаю"), with the AI-review verdicts.
> Written by Claude Code from the decision list she approved; revised after the AI design
> review (`docs/m4/ai-review.md`).

## Context

- M3: a coordinator routes each key to one of 2 shards; each shard is a replica set of 3
  (one per AZ) with a single leader, W = 2, R = 2. Replicas heartbeat every 5 s; a replica
  silent for 15 s is down; a dead leader is replaced automatically (ADRs 0020–0023). A
  read replaces a replica that doesn't answer (3 s client timeout) with the next live one.
- What exists for observability: `/health` on the coordinator (503/504 counts) and on each
  replica (role, epoch, last LSN, lag, commit-wait p99); plain-text logs; nothing collects
  them.
- Sandbox limits (`docs/m1/labrole-dissection.md`): CloudWatch custom metrics, dashboards
  and X-Ray are denied; our instances can't send metrics or logs to CloudWatch, publish to
  SNS or write to S3; they can read Secrets Manager. Private subnets have no internet route
  (ADR-0005); public subnets do.
- $32.7 of the $60 credit is left; the lab's own NAT Gateway uses ≈ $1.20 a day of it.

## Goals / Non-Goals

**Goals:** latency, throughput and errors per operation, shard and replica; one request
followed through the logs, the follower's side included; a dashboard with cluster health,
replication lag and shard distribution; alerts for a node or replica down, the coordinator
down, p99 above the SLO and replication lag, delivered to a phone, and noticed when the
monitor itself stops; record versions with last-write-wins that also cover retries; an MCP
server whose limits the store enforces.

**Non-Goals:** distributed tracing with spans, vector clocks, host metrics
(`node_exporter`), per-table keys and rate limits (Bonus 1), the optional automation,
autoscaling and rebalancing (M5).

## Decisions

1. **Metrics:** Prometheus text format written with the standard library (ADR-0009), on a
   separate metrics port; SLIs are availability and p99 at the coordinator, per shard, plus
   an outside probe. ADR-0025.
2. **SLOs:** availability ≥ 99.5% per hour of operation; read p99 ≤ 15 ms, write p99 ≤
   60 ms over 5 minutes, per shard. ADR-0026.
3. **MCP:** a Go binary on the official SDK, stdio; read, exists, create-only, conditional
   update and delete; the store enforces the limits through API keys with scopes. ADR-0027.
4. **Versions:** an HLC stamp per record, issued by the leader and returned on every write
   answer; last-write-wins per key; the stamp is the ETag for `If-Match`. ADR-0028.
5. **Stack:** Prometheus, Alertmanager, Grafana, Loki and a blackbox probe on one `monitor`
   instance; Grafana Alloy ships logs from every instance; alerts to Telegram. ADR-0029.

## Architecture

```
  laptop ──SSM port forwarding──▶ Grafana :3000, Prometheus :9090      (no public ingress)

  ┌ monitor (public subnet A, optional) ─────────────────────────────────────────────┐
  │ Prometheus ──rules──▶ Alertmanager ──▶ Telegram     blackbox probe ──▶ coordinator │
  │     │ scrape every 5 s                                Loki ◀── push ── Alloy       │
  └─────┼────────────────────────────────────────────────────────────────────────────┘
        ▼
  coordinator  API :8080   metrics :9180
  node a/b/c   replicas :8081/:8082   metrics :9181/:9182   (the monitor reaches only 918x)
```

## Metrics

| Metric | Where | Labels | SLI? |
| --- | --- | --- | --- |
| `kv_requests_total` | coordinator | `op` (read, exists, write, delete, table), `code`, `shard`, `principal` | **yes**: availability = non-5xx / all |
| `kv_request_duration_seconds` (histogram) | coordinator | `op`, `shard` | **yes**: read and write p99 |
| `kv_probe_success`, `kv_probe_duration_seconds` | blackbox probe (monitor) | — | **yes**: availability seen from outside, also when the coordinator is down |
| `kv_replica_up`, `kv_shard_has_leader`, `kv_shard_leader_epoch` | coordinator | `shard`, `replica`, `node` | no: health, alerts |
| `kv_replica_last_lsn` | coordinator (from heartbeats) | `shard`, `replica` | no: dashboard |
| `kv_replica_request_duration_seconds` (histogram), `kv_replica_errors_total` | replica | `op` | no: which replica is slow or failing |
| `kv_replica_role`, `kv_store_keys`, `kv_log_bytes`, `kv_replica_reads_total` | replica | — | no: shard distribution, read balance |
| `kv_commit_wait_seconds` (histogram) | leader | — | no: explains write p99 |
| `kv_follower_acked_lsn`, `kv_follower_lag_seconds` | leader | `follower` | no: the lag alert's source |
| `go_goroutines`, `go_gc_pause_seconds`, `go_heap_bytes` | every process | — | no: runtime health |

Every replica metric also carries `shard` and `replica` labels. `kv_follower_lag_seconds`
is the time since the follower's last pull that reached the leader's last record, measured
on the leader, so it keeps growing when a follower is wedged or cut off.

## SLOs and alerts

Prometheus scrapes and evaluates rules every 5 s. Alertmanager: `group_wait` 5 s,
`group_interval` 30 s, `repeat_interval` 1 h, grouped by `alertname` and `node`.

| Alert | Condition | For | Severity |
| --- | --- | --- | --- |
| `CoordinatorDown` | probe fails, or `up{job="coordinator"} == 0` | 10 s | critical |
| `NodeDown` | every replica on a node has `kv_replica_up == 0` | — | critical |
| `ReplicaDown` | `kv_replica_up == 0` | — | warning |
| `ShardWithoutLeader` | `kv_shard_has_leader == 0` | 30 s | critical |
| `ScrapeFailing` | `up == 0` for a replica target | 30 s | warning |
| `MetricsMissing` | `absent(kv_replica_up)` while the coordinator is up | 30 s | critical |
| `ReadLatencyHigh` / `WriteLatencyHigh` | p99 of a shard above 15 / 60 ms, at ≥ 1 request/s | 5 min | warning |
| `ReplicationLagHigh` | `kv_follower_lag_seconds > 10` | 30 s | warning |
| `WritesNotCommitted` | 504 rate above 0.1/s | 1 min | warning |
| `ErrorBudgetFastBurn` | 5xx above 5% over 5 min **and** over 1 min | — | critical |
| `ErrorBudgetSpent` | 5xx above 0.5% over 1 h | — | warning |
| `Watchdog` | always firing; repeated hourly to Telegram | — | info |

`Watchdog` is the monitor's own heartbeat: an hour without it means the monitor is down.

**Detection of a stopped node:** the coordinator marks its replicas down after 15 s of
silence, Prometheus sees it within one scrape (≤ 5 s), the rule fires at the next
evaluation, Alertmanager waits `group_wait`: about 25 s, plus Telegram delivery. Reported as
measured in task 8.2, not as this estimate.

## Requests through the logs

- Every log line is JSON (`log/slog`): time, level, message, `request_id`, `principal`
  (the API key's name), role, shard, replica, operation, status, duration. No values and no
  API keys are logged; keys appear as a hash of table and key.
- The coordinator takes `X-Request-ID` from the client or makes one, logs it, sends it to
  every replica it calls, and returns it.
- The leader logs the request ID with the LSN it assigned; a follower logs the LSN ranges it
  appends and applies. So one request is followed coordinator → leader (commit wait) →
  followers (by LSN).
- On R = 2 reads the coordinator logs each replica's time.
- Not covered: spans with per-hop timings, the gap named for the incident-investigation
  question.

## Versions and last-write-wins

```
record (format 2) = format 2 | length | epoch | LSN | prev hash | HLC stamp (8 B) | op | key | value | CRC
HLC stamp = physical ms (48 bits) | logical counter (16 bits)
```

- **The leader stamps every write** with `max(now, last stamp + 1)`; a new leader starts
  above its log's last stamp. Within a shard stamps are unique and rise in log order.
- **Every write answer carries the stamp** (`X-KV-Timestamp`), the 504 included: the leader
  has appended the record before the commit wait times out.
- **A retry sends that stamp back** in `X-KV-Timestamp`. A client may send any stamp, but
  one more than 5 s ahead of the leader's **physical** clock gets 400; the leader moves its
  HLC forward to an accepted stamp.
- **Last-write-wins:** if the key's latest record (committed or not) has a stamp ≥ the
  write's, the write isn't appended. Its answer waits until that latest record has
  committed, then is 200 with `X-KV-Superseded: true`; if the commit times out, 504.
- Without a stamp from the client, a write is newer than everything before it: "last
  arrival wins". LWW changes the outcome only for a write that carries an earlier stamp, a
  retry above all.
- **Reads** still pick the newest `(epoch, LSN)` among R replicas; within a shard that
  order and the stamp order agree. Reads return `ETag: "<stamp>"` and `X-KV-Timestamp`.
- **Conditional writes:** `If-Match: "<stamp>"` on PUT or DELETE: 412 if the key's latest
  stamp differs. `If-None-Match: *` on PUT: 412 if the key exists (create only).
- **M5:** key migration compares stamps across shards with the shard ID as a tie-breaker
  and applies the receive rule on the target leader.
- **Old logs:** format 1 records (M3) are still read, with stamp 0.

## API keys

| Scope | Allows |
| --- | --- |
| `read` | GET and HEAD on items, list and describe tables |
| `write` | PUT on items |
| `delete` | DELETE on items |
| `admin` | create tables |

`Authorization: Bearer <key>` on every public route; 401 without a valid key, 403 without
the scope. Keys are stored as SHA-256 hashes with a name and scopes in a Secrets Manager
secret (`velmakina-kv/api-keys`), written to a file on the coordinator at boot; locally a
file. The coordinator reloads the file when it changes, so a key is rotated without a
restart (`make rotate-keys` refreshes the file through SSM). `/health`, the metrics port
and the internal API need no key: none returns item data to the monitor, and the internal
API is reachable only between the coordinator's and the replicas' security groups.

The smoke test and the benchmark client need a key in plain text: an `ops` key with every
scope, in a second secret (`velmakina-kv/ops-key`), read at run time by the instance that
runs them. The probe on the monitor uses a separate `probe` key with `read` only, from the
same secret, so a compromised monitor can't change data. The pipeline never sees either.

## MCP server

| Tool | Store call | Scope | Notes |
| --- | --- | --- | --- |
| `list_tables` | GET /v1/tables | read | read-only hint |
| `read_item` | GET | read | value as data, at most 16 KB, with its stamp |
| `item_exists` | HEAD | read | read-only hint |
| `create_item` | PUT + `If-None-Match: *` | write | the brief's CREATE: fails if the key exists |
| `update_item` | PUT + `If-Match` | write | needs the stamp from a read; fails if the item changed |
| `delete_item` | DELETE | delete | destructive hint (advisory) |

Results name the outcome: created, updated, superseded, conflict (412), not found, not
permitted (401/403), invalid (400), unavailable (503), outcome unknown (504). Not exposed:
table creation, the internal API, anything outside the store. The server holds one key from
its environment; the default key has `read` and `write`, so an agent can't delete and can't
overwrite an item it hasn't read.

## Local and AWS

- `docker-compose.yml`: the M3 cluster plus Prometheus, Alertmanager, Grafana, Loki, the
  blackbox exporter and Alloy, with the same configuration files as in AWS.
- AWS: `infra/app.yaml` gets `Monitoring` (true/false) like `BenchmarkClient`. The monitor
  pulls its images from the internet (public subnet); Alloy's image reaches the private
  nodes through the artifacts bucket, as the app image does.
- In M5 the monitor's Prometheus becomes the autoscaler's input, so the monitor stops being
  optional there. Before every teardown the evidence (query results, dashboard screenshots)
  is exported from the laptop through SSM port forwarding.

## Risks / Trade-offs

- [The monitor is one more thing to run and pay for] → only during sessions; ≈ $0.02/h.
- [Instrumentation adds latency] → one benchmark run of workload A compared with the M3
  control.
- [A stopped node makes reads wait out the 3 s client timeout until it is marked down]
  → measured in the drill (5xx and latency); ADR-0026 corrected if the cost is higher than
  assumed.
- [An API key leaks] → hashed at rest, scoped, logged only by name, rotated without a
  restart.
- [Text stored in an item talks an agent into damage] → the default key can't delete and
  can't overwrite without the item's current stamp; the destructive hint is a second line.
