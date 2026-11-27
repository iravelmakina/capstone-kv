# Spec Delta

## Purpose

Every process says whether it is alive, whether it can serve, and exactly which build it
runs, in a form scripts, the executor, the agent and the monitor can all read.

## ADDED Requirements

### Requirement: Liveness
The coordinator and every replica SHALL answer `GET /health/live` with 200 and
`{"status":"ok"}` while the process runs, without checking other components and without an
API key.

#### Scenario: Live while not ready
- **WHEN** the coordinator has started but fewer than the expected shards have registered
- **THEN** `/health/live` answers 200 and `/health/ready` answers 503

### Requirement: Readiness
`GET /health/ready` SHALL answer 200 only when the process can serve its role: the
coordinator when every shard on the ring has a leader and at least 2 live replicas; a
replica when its log has been replayed and it knows its leader and epoch. Otherwise 503,
with the same JSON fields and a reason. `GET /health` SHALL keep its M4 behaviour and body.

#### Scenario: Replica replaying its log
- **WHEN** a replica is still replaying its log at start
- **THEN** `/health/ready` answers 503 with reason "replaying"

### Requirement: Build version
The coordinator and every replica SHALL answer `GET /version` with 200 and
`{"version": <commit SHA>, "buildTime": <RFC 3339>, "goVersion": ..., "role": ...}`, and a
replica also with its shard and replica ID, without an API key. The values SHALL be those
compiled into the binary, not taken from configuration. Every process SHALL export
`kv_build_info{version,role}` with the value 1.

#### Scenario: Release check
- **WHEN** the app stack runs release `abc123def456`
- **THEN** `/version` on the coordinator and every replica returns `"version":"abc123def456"`, and `count by (version) (kv_build_info)` has a single version

### Requirement: No data or secrets in health answers
Health and version answers SHALL contain no item values, keys of items, API keys or
secret names.

#### Scenario: Health body checked
- **WHEN** the health and version answers of every process are read
- **THEN** none of them contains an item value, an item key or a credential
