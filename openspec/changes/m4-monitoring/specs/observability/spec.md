# Spec Delta

## Purpose

The store's health, latency, errors and replication are measured, visible on a dashboard,
and alerted on, also when the coordinator or the monitor itself is down; one request can be
followed through the logs.

## ADDED Requirements

### Requirement: Metrics endpoint
The coordinator and every replica SHALL serve `/metrics` in the Prometheus text format on a
port separate from the API, without an API key, and every process SHALL include Go runtime
metrics.

#### Scenario: Metrics port only
- **WHEN** the monitor connects to a replica's API port
- **THEN** the connection is refused by the security group, while its metrics port answers

### Requirement: Coordinator metrics
The coordinator SHALL count requests by operation, status code, shard and principal, and
record their latency in a histogram by operation and shard.

#### Scenario: Request counted
- **WHEN** a client reads an existing key
- **THEN** the coordinator's `kv_requests_total{op="read",code="200"}` for that key's shard and the client's principal grows by one

### Requirement: Replica metrics
Each replica SHALL record its own request latency and errors by operation, and report its
role, number of keys and log size; a leader SHALL report each follower's acknowledged LSN
and the seconds since that follower last caught up.

#### Scenario: Lag visible on the leader
- **WHEN** a follower is stopped while writes continue for 20 seconds
- **THEN** the leader's `kv_follower_lag_seconds` for that follower rises above 15, and returns to 0 after the follower is started and has caught up

### Requirement: SLIs and SLOs
The SLIs SHALL be availability (the share of public requests answered with a status below
500, at the coordinator, and the share of successful probes from the monitor) and p99
latency of reads and of writes per shard at the coordinator. The SLOs SHALL be
availability ≥ 99% per hour of operation, read p99 ≤ 15 ms and write p99 ≤ 60 ms over 5
minutes, per shard.

#### Scenario: SLO on the dashboard
- **WHEN** the dashboard is opened during a benchmark run
- **THEN** it shows availability and each shard's read and write p99 against their SLO lines

#### Scenario: Outage seen from outside
- **WHEN** the coordinator is stopped
- **THEN** the probe's success ratio drops to 0 and the availability panel shows it

### Requirement: Request IDs in logs
Every log line SHALL be JSON. The coordinator SHALL take `X-Request-ID` from the client or
create one, pass it to every replica it calls for that request, log it with the caller's
key name, and return it in the response. The leader SHALL log the LSN it assigned to the
request; followers SHALL log the LSN ranges they append. Logs SHALL contain no item values
and no API keys.

#### Scenario: One write followed
- **WHEN** a client writes a key and searches the logs for the request ID returned to it
- **THEN** it finds the coordinator's line and the leader's line with the LSN, and by that LSN the followers' lines

### Requirement: Dashboard
A Grafana dashboard, provisioned from a file in the repository, SHALL show replicas and
nodes up, each shard's leader and epoch, throughput and error rate by operation, p50, p95
and p99 latency per shard, each replica's latency, replication lag, commit wait, keys per
shard, and the probe's availability.

#### Scenario: Shard distribution
- **WHEN** 10,000 keys have been written
- **THEN** the dashboard shows the keys per shard, each within 10% of half the total

### Requirement: Alert delivery
Alert rules SHALL be files in the repository, tested with `promtool`, evaluated every 5
seconds, and delivered by Alertmanager to a Telegram chat; an always-firing watchdog alert
SHALL be repeated there every hour.

#### Scenario: Monitor silent
- **WHEN** the monitor runs
- **THEN** a watchdog message reaches the Telegram chat every hour, so an hour of silence means the monitor is down

### Requirement: Alert coverage
Alerts SHALL fire for: the coordinator down (probe or scrape), a node down, a replica down,
a shard without a leader, metrics missing, a shard's read or write p99 above its SLO at ≥ 1
request per second, replication lag above 10 seconds, a sustained 504 rate, a fast
error-budget burn (5xx above 10% over both 5 minutes and 1 minute), and the hourly budget
spent (5xx above 1%). The two budget alerts SHALL fire only while the store gets at least 1
request per second over 5 minutes.

#### Scenario: No budget page at idle
- **WHEN** the store gets almost no traffic and a few probes fail
- **THEN** neither `ErrorBudgetFastBurn` nor `ErrorBudgetSpent` fires

#### Scenario: Node down alerted
- **WHEN** a shard node that runs no leader is stopped
- **THEN** a `NodeDown` alert naming that node reaches the Telegram chat within 60 seconds, and a resolved message follows after the node is started and has caught up

#### Scenario: Leader lost
- **WHEN** a node running a leader is stopped
- **THEN** `NodeDown` fires, the dashboard shows a new leader under a higher epoch, and no `ShardWithoutLeader` alert stays firing after promotion

#### Scenario: Coordinator down
- **WHEN** the coordinator's container is stopped
- **THEN** a critical `CoordinatorDown` alert reaches the Telegram chat within 60 seconds
