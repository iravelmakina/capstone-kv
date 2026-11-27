# Proposal

> **Approved** by the author on 2026-11-27 ("я все приймаю"). Written by Claude Code from
> the decision list she approved; revised after the AI design review
> (`docs/m4/ai-review.md`).

## Why

M3 made the store survive the loss of a replica, but nobody sees what it is doing: the only
signals are `/health` endpoints read by hand and benchmark summaries. M4 adds the
observability needed to notice and handle failures (metrics, logs with request IDs, a
dashboard, alerts), gives records a timestamp so conflicting writes resolve by
last-write-wins, and opens the store to AI agents through an MCP server with clear limits.

## What Changes

- **Metrics:** the coordinator and every replica serve `/metrics` in the Prometheus text
  format on a separate port: request counts and latency histograms by operation, shard and
  replica, errors, replication lag measured on the leader, commit wait, leader and epoch,
  keys per shard, Go runtime. Availability and p99 per shard at the coordinator, plus an
  outside probe, are the SLIs.
- **SLOs:** availability ≥ 99% per hour (ADR-0030) and p99 ≤ 15 ms for reads, ≤ 60 ms for writes
  per shard, measured at the coordinator.
- **Logs:** JSON logs with a request ID that the coordinator assigns and passes to the
  replicas, and the key's name as `principal`; the leader logs the LSN it gave the request,
  so one write is followed from the coordinator to the leader and the followers.
- **Monitoring stack:** Prometheus, Alertmanager, Grafana, Loki and a blackbox probe on one
  `monitor` instance in a public subnet, started only during lab sessions, reached through
  SSM port forwarding; Grafana Alloy ships logs from every instance. Dashboards and alert
  rules are files in the repo.
- **Alerts:** the coordinator down, a node or replica down, a shard without a leader, p99
  above the SLO, replication lag, commit timeouts, error-budget burn, and a watchdog that
  shows the monitor is alive; delivered to a Telegram chat.
- **Record versions:** every record gets a hybrid logical clock (HLC) stamp from its leader
  (record format version 2), returned on every write answer, the 504 included. A retry
  that sends its stamp back loses to any newer write (last-write-wins). Conditional writes
  (`If-Match`, `If-None-Match: *`) are the client-side strategy.
- **API keys:** the coordinator requires a key with scopes (`read`, `write`, `delete`,
  `admin`); keys live in Secrets Manager and are reloaded without a restart.
- **MCP server:** a separate binary (`cmd/kvmcp`) exposing EXISTS, CREATE (create only),
  a conditional UPDATE, READ and DELETE as tools over stdio; it holds its own key; the
  default key can't delete, and DELETE is marked destructive.
- **ADRs:** 0025 metrics and SLIs, 0026 SLOs, 0027 MCP boundaries and authorisation,
  0028 record versions and last-write-wins, 0029 monitoring stack and logs.

## Capabilities

### New Capabilities

- `observability`: metrics, SLIs and SLOs, request IDs in logs, dashboard, alerts.
- `record-versions`: HLC timestamps per record, last-write-wins, conditional writes.
- `api-keys`: keys with scopes on the public API.
- `mcp-server`: the store's tools for AI agents and their limits.

### Modified Capabilities

- `kv-api` (M2) and `replication` (M3): writes carry a timestamp, a superseded write is
  reported as such, and every request needs a key. Written as new requirements in the
  capabilities above, since the M2 and M3 changes aren't archived yet.

## Non-Goals

- Distributed tracing with spans (request IDs in logs only); listed as the gap for the
  control question on incident investigation.
- Host metrics (`node_exporter`), per-table keys and per-key rate limits (Bonus 1).
- Vector clocks and leaderless replication: one leader per shard orders writes already.
- The optional operational automation (incident tickets, backups, retention, reports).
- Autoscaling and rebalancing (M5); the HLC timestamp is what M5's key migration will
  compare.

## Impact

- Code: `internal/store` (record format 2), `internal/shard`, `internal/coordinator`
  (metrics, keys, request IDs, stamps), new `internal/metrics`, new `internal/auth`, new
  `cmd/kvmcp`, `api/openapi.yaml`, `api/internal-openapi.yaml`.
- Infra: `infra/app.yaml` gets an optional monitor instance and its security group; the
  replicas' and the coordinator's metrics ports admit the monitor; three Secrets Manager
  secrets (API key hashes, the ops key, the Telegram bot and Grafana password) created by
  hand, never in the repo.
- Local: `docker-compose.yml` gains the monitoring stack.
- AWS: one more t3.small while monitoring is on (≈ $0.02/h); $32.7 of the credit is left.
