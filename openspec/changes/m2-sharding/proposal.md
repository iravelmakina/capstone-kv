# Proposal

> **Approved** by the author on 2026-11-24. Drafted with Claude Code from the decision list
> agreed in chat.

## Why

M2 turns the M1 network into a working, sharded key-value store: a coordinator that owns
the API and routes each key to one of several shards through a consistent hashing ring,
deployed by a pipeline that carries the project to M5, with a YCSB baseline that M3 and M5
compare against.

## What Changes

- **API:** REST + JSON, specified in `api/openapi.yaml`: register a table (partition key,
  optional sort key), and CREATE / READ / EXISTS / DELETE on its items.
- **Coordinator:** serves the API, keeps the table registry, hashes `table + partition key`
  onto a ring with virtual nodes, forwards to the owning shard, registers shards as they
  start and heartbeat.
- **Shard:** stores keys in memory with an append-only log on disk, replayed at start.
- **Delivery:** one Go binary with two roles, shipped as a Docker image through S3.
- **Infrastructure:** two new stacks, `velmakina-kv-artifacts` (S3) and `velmakina-kv-app`
  (coordinator + 2 shards + optional benchmark client, security groups), importing the M1
  network's exports.
- **CI/CD:** GitHub Actions: test → build → deploy infra → deploy app → smoke test; a
  separate manual benchmark workflow.
- **Benchmark:** go-ycsb with our binding, workloads A and B, results kept as the baseline.
- **ADRs:** 0009 runtime and delivery, 0010 API protocol, 0011 hash and virtual nodes,
  0012 partition key, 0013 shard storage and membership, 0014 app infrastructure,
  0015 CI/CD pipeline, 0016 benchmark.

## Capabilities

### New Capabilities

- `kv-api`: the public API and its contract.
- `sharding`: the ring, routing and shard membership.
- `shard-storage`: how a shard keeps data across restarts.
- `app-deployment`: the app and artifacts stacks and how an image reaches the instances.
- `delivery-pipeline`: the CI/CD workflows.
- `benchmark-baseline`: the YCSB baseline.

### Modified Capabilities

None. The M1 network is used through its exports, unchanged.

## Impact

- New code: `cmd/kvnode/`, `internal/`, `api/openapi.yaml`, `bench/`, `Dockerfile`,
  `docker-compose.yml`, `.github/workflows/`.
- New IaC: `infra/artifacts.yaml`, `infra/app.yaml`; new `Makefile` targets.
- AWS: 3–4 × t3.small while a session runs (≈ $0.08/h), an S3 bucket; nothing left
  running between sessions.
- New: a GitHub repository (`iravelmakina/capstone-kv`) with lab credentials as secrets.
