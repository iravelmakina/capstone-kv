# capstone-kv

A small DynamoDB-like key-value store, built over five milestones for the KSE Cloud
Architectures course (2026/27), in the AWS Academy sandbox. Course brief:
[`docs/course/capstone-brief.md`](docs/course/capstone-brief.md).

**Current milestone: M2 — sharding the load.** Spec: OpenSpec change
[`m2-sharding`](openspec/changes/m2-sharding/proposal.md) (M1 is archived in
[`openspec/changes/archive/`](openspec/changes/archive/), its requirements are the base in
[`openspec/specs/`](openspec/specs/)). Decisions: [`docs/adr/`](docs/adr/) (numbering starts
at 0002: ADR-0001 belonged to lab 1, outside this repo).

## What it is

```
client ──HTTP──▶ coordinator ──▶ shard-1   (private subnet, AZ a)
                 (public subnet)  shard-2   (private subnet, AZ b)
```

- **API:** REST + JSON, [`api/openapi.yaml`](api/openapi.yaml): register a table (partition
  key, optional sort key), then CREATE (`PUT`), READ (`GET`), EXISTS (`HEAD`), DELETE on
  `/v1/tables/{table}/items/{pk}[/{sk}]`.
- **Coordinator:** routes each item by `table + partition key` on a consistent hashing ring
  (xxHash64, 128 virtual nodes); shards register at start and send heartbeats.
- **Shard:** keys in memory plus an append-only log on disk, replayed after a restart.
- One Go binary for both roles, shipped as a Docker image through S3; CloudFormation stacks;
  GitHub Actions pipeline; YCSB baseline.

## Where things are

| Item | Where |
| --- | --- |
| M2 decisions | [ADR-0009](docs/adr/0009-runtime-and-delivery.md) runtime and delivery, [0010](docs/adr/0010-api-protocol.md) API, [0011](docs/adr/0011-hash-and-virtual-nodes.md) hash and virtual nodes, [0012](docs/adr/0012-partition-key-strategy.md) partition key, [0013](docs/adr/0013-shard-storage-and-membership.md) storage and membership, [0014](docs/adr/0014-app-infrastructure.md) app infrastructure, [0015](docs/adr/0015-ci-cd-pipeline.md) CI/CD, [0016](docs/adr/0016-benchmark.md) benchmark; after the AI review: [0017](docs/adr/0017-pinned-ami.md) pinned AMI, [0018](docs/adr/0018-log-damage-and-failed-writes.md) damaged logs and failed writes |
| Code | [`cmd/kvnode`](cmd/kvnode/main.go), [`internal/`](internal/) (`ring`, `store`, `shard`, `coordinator`), [`bench/`](bench/) (go-ycsb binding) |
| Infrastructure | [`infra/network.yaml`](infra/network.yaml) (M1), [`infra/artifacts.yaml`](infra/artifacts.yaml), [`infra/app.yaml`](infra/app.yaml), [`Makefile`](Makefile) |
| Pipeline | [`.github/workflows/ci-cd.yml`](.github/workflows/ci-cd.yml), [`benchmark.yml`](.github/workflows/benchmark.yml); runs in [`docs/m2/pipeline.md`](docs/m2/pipeline.md) |
| Evidence | [ring distribution](docs/m2/ring-distribution.md), [benchmark baseline](docs/m2/benchmark.md), [local Docker run](docs/m2/evidence/local-docker.txt), [first AWS deploy](docs/m2/evidence/aws-deploy.txt), [actual spend](docs/m2/cost.md) |
| Defense prep | [control questions](docs/m2/control-questions.md), [AI design review](docs/m2/ai-review.md) |
| M1 | [`docs/m1/`](docs/m1/): LabRole dissection, IAM template, quotas, budget, recreate log |

## Prerequisites

- Go 1.24, Docker, `make`.
- For AWS: an AWS Academy lab session. Start the lab, open **AWS Details → AWS CLI: Show**,
  put the block under `[academy]` in `~/.aws/credentials` (`region = us-east-1` under
  `[profile academy]` in `~/.aws/config`). Credentials expire with each 3-hour session.
- AWS CLI and `cfn-lint`: from PATH, or in the repo's git-ignored virtualenv, which the
  Makefile picks up by itself:
  ```bash
  python3 -m venv .venv && .venv/bin/pip install cfn-lint awscli
  ```

## Run it locally

```bash
make test         # go vet + all tests (unit, contract test against openapi.yaml, ring distribution)
make local-up     # coordinator + 2 shards in Docker (the same image as in AWS)
make smoke        # create table, put, read, exists, delete against http://localhost:8080
make local-down
```

By hand:

```bash
curl -X POST localhost:8080/v1/tables -d '{"name":"users","partitionKey":"userId"}'
curl -X PUT  localhost:8080/v1/tables/users/items/u1 -d '{"name":"Ira"}'
curl         localhost:8080/v1/tables/users/items/u1
curl -I      localhost:8080/v1/tables/users/items/u1      # EXISTS
curl -X DELETE localhost:8080/v1/tables/users/items/u1
curl         localhost:8080/internal/v1/shards            # ring membership
```

## Deploy to AWS

Normally the pipeline does it: a push to `main` runs tests, builds the release (named by the
commit), deploys the stacks through change sets and smoke-tests the result. The same steps
by hand:

```bash
make whoami        # assumed-role/voclabs/...
make up            # network stack (M1), if it isn't there
make up-artifacts  # S3 bucket for releases (once)
make release       # build the image of the current commit and upload it
make up-app        # coordinator + 2 shards running that release
make smoke-aws     # smoke test on the coordinator through SSM
make app-url       # coordinator URL (reachable from your IP only)
make down-app      # at the end of every lab session: instances cost money
```

Rollback: run the **ci-cd** workflow by hand with an earlier release SHA, or
`make up-app VERSION=<sha>`.

## Verify it

| Check | How |
| --- | --- |
| Code and contract | `make test` (CI runs it on every push) |
| Templates | `make lint`; `make verify` (no internet route in private subnets); `make drift` |
| Deployed app | `make smoke-aws`: all four operations, and `/health` reports the deployed version |
| Behaviour on failure | [`docs/m2/evidence/local-docker.txt`](docs/m2/evidence/local-docker.txt): shard restart replays its log; a stopped shard's keys answer 503, the rest keep working; coordinator restart waits for the shards |
| Performance | `make up-app BENCH=true && make bench-aws && make down-app`, or the **benchmark** workflow |

The scenarios each check must satisfy are in the
[M2 specs](openspec/changes/m2-sharding/specs/).

## What changed since M1

- **New:** the store itself: API contract, coordinator, shards, consistent hashing ring,
  durable shard storage, one Docker image for both roles.
- **New stacks:** `velmakina-kv-artifacts` (S3, long-lived) and `velmakina-kv-app`
  (coordinator, 2 shards, security groups, optional benchmark client). They import the M1
  network's exports; the network stack itself is unchanged.
- **New:** a CI/CD pipeline (GitHub Actions) and a separate benchmark workflow; the
  repository is public at https://github.com/iravelmakina/capstone-kv.
- **Changed:** the Makefile deploys any stack with parameters, `make down` deletes the app
  before the network, and tools can come from `.venv/`.
- **Results:** YCSB baseline A 1,804 ops/s, B 6,891 ops/s (2 shards, t3.small).

## How this repo is worked on

Spec before code: every milestone starts with an OpenSpec change and ADRs, committed before
any template or code. AI assistance (Claude Code) is logged in
[`AGENT_WORKLOG.md`](AGENT_WORKLOG.md).
