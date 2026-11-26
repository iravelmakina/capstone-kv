# capstone-kv

A small DynamoDB-like key-value store, built over five milestones for the KSE Cloud
Architectures course (2026/27), in the AWS Academy sandbox. Course brief:
[`docs/course/capstone-brief.md`](docs/course/capstone-brief.md).

**Current milestone: M3 — durability with replication.** Spec: OpenSpec change
[`m3-replication`](openspec/changes/m3-replication/proposal.md), built on M2's
[`m2-sharding`](openspec/changes/m2-sharding/proposal.md) (archived after its defense, task 1.1). M1 is
archived in [`openspec/changes/archive/`](openspec/changes/archive/), its requirements are the
base in [`openspec/specs/`](openspec/specs/). Decisions: [`docs/adr/`](docs/adr/) (numbering starts
at 0002: ADR-0001 belonged to lab 1, outside this repo).

## What it is

```
                                      node a (AZ 1)   node b (AZ 2)   node c (AZ 4)
client ──HTTP──▶ coordinator ──▶ shard-1:  1a leader        1b              1c
                 (public subnet)  shard-2:  2a               2b leader       2c
                                  (private subnets; followers pull the leader's log)
```

- **API:** REST + JSON, [`api/openapi.yaml`](api/openapi.yaml): register a table (partition
  key, optional sort key), then CREATE (`PUT`), READ (`GET`), EXISTS (`HEAD`), DELETE on
  `/v1/tables/{table}/items/{pk}[/{sk}]`. Coordinator–shard endpoints (registration,
  storage) are in [`api/internal-openapi.yaml`](api/internal-openapi.yaml), not the public contract.
- **Coordinator:** routes each item by `table + partition key` on a consistent hashing ring
  (xxHash64, 128 virtual nodes) to a shard; writes go to the shard's leader, reads to R
  replicas in parallel (newest version wins). Replicas heartbeat every 5 s; the coordinator
  keeps each shard's leader and epoch in its own log and promotes a follower when a leader
  misses 3 heartbeats.
- **Shard = replica set of 3** (N = 3, W = 2, R = 2 by default; W = 1 with R = 3 is a
  setting). Each replica keeps keys in memory plus an append-only log on disk; records carry
  `(epoch, LSN)`, a hash link to the previous record and a checksum. Followers long-poll the
  leader's log, verify it, fsync and apply; a write commits once one follower has it on disk.
- One Go binary for both roles, shipped as a Docker image through S3; CloudFormation stacks;
  GitHub Actions pipeline; YCSB baseline.

## Where things are

| Item | Where |
| --- | --- |
| M3 decisions | [ADR-0020](docs/adr/0020-replication-model-and-placement.md) replication model, placement and leader changes, [0021](docs/adr/0021-n-w-r-values.md) N / W / R, [0022](docs/adr/0022-log-structure-and-ordering.md) log structure and ordering, [0023](docs/adr/0023-replica-sync.md) replica sync and rejoin |
| M2 decisions | [ADR-0009](docs/adr/0009-runtime-and-delivery.md) runtime and delivery, [0010](docs/adr/0010-api-protocol.md) API, [0011](docs/adr/0011-hash-and-virtual-nodes.md) hash and virtual nodes, [0012](docs/adr/0012-partition-key-strategy.md) partition key, [0013](docs/adr/0013-shard-storage-and-membership.md) storage and membership, [0014](docs/adr/0014-app-infrastructure.md) app infrastructure, [0015](docs/adr/0015-ci-cd-pipeline.md) CI/CD, [0016](docs/adr/0016-benchmark.md) benchmark; after the AI review: [0017](docs/adr/0017-pinned-ami.md) pinned AMI, [0018](docs/adr/0018-log-damage-and-failed-writes.md) damaged logs and failed writes |
| Code | [`cmd/kvnode`](cmd/kvnode/main.go), [`internal/`](internal/) (`ring`, `store`, `shard` with [`replica.go`](internal/shard/replica.go), `coordinator` with [`membership.go`](internal/coordinator/membership.go)), [`bench/`](bench/) (go-ycsb binding) |
| Infrastructure | [`infra/network.yaml`](infra/network.yaml) (M1), [`infra/artifacts.yaml`](infra/artifacts.yaml), [`infra/app.yaml`](infra/app.yaml), [`Makefile`](Makefile) |
| Pipeline | [`.github/workflows/ci-cd.yml`](.github/workflows/ci-cd.yml), [`benchmark.yml`](.github/workflows/benchmark.yml); runs in [`docs/m2/pipeline.md`](docs/m2/pipeline.md) |
| M3 evidence | [local failure tests](docs/m3/evidence/local-docker.txt), [AWS sessions](docs/m3/evidence/aws-deploy.txt), [benchmark vs M2](docs/m3/benchmark.md), [spend](docs/m3/cost.md) |
| M3 defense prep | [control questions](docs/m3/control-questions.md), [AI design review](docs/m3/ai-review.md) |
| M2 evidence | [ring distribution](docs/m2/ring-distribution.md), [benchmark baseline](docs/m2/benchmark.md), [local Docker run](docs/m2/evidence/local-docker.txt), [first AWS deploy](docs/m2/evidence/aws-deploy.txt), [actual spend](docs/m2/cost.md) |
| M2 defense prep | [control questions](docs/m2/control-questions.md), [AI design review](docs/m2/ai-review.md) |
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
make local-up     # coordinator + 2 shards x 3 replicas in Docker (the same image as in AWS)
make smoke        # create table, put, read, exists, delete against http://localhost:8080
make local-down   # data stays in named volumes; `docker compose down -v` wipes it
```

Replication by hand (replica health on `127.0.0.1:8111-8113` for shard-1, `8121-8123` for
shard-2):

```bash
curl localhost:8080/internal/v1/shards     # leader, epoch, each replica's last (epoch, LSN)
curl 127.0.0.1:8112/health                 # one replica: role, epoch, last LSN, lag, commit wait
docker compose stop r1c                    # a follower down: puts and reads still work
docker compose start r1c                   # it pulls what it missed
make rebuild-replica R=1b                  # wipe a replica and resync it from the leader
scripts/smoke-replica.sh http://localhost:8080 pick   # a replica that is no leader
```

All failure scenarios of the M3 specs (follower down, both down, leader replaced, former
leader returns, broken hash link, rebuild, coordinator restart, balanced reads), on a fresh
cluster:

```bash
docker compose down -v && make local-up && scripts/failure-tests.sh
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
make up-app        # coordinator + 3 nodes (2 shards x 3 replicas) running that release; W=1 for the async variant
make smoke-aws     # smoke test on the coordinator through SSM
make smoke-replica # stops a follower-only node, checks puts still commit, starts it again
make app-url       # coordinator URL (reachable from your IP only)
make down-app      # at the end of every lab session: instances cost money
```

Rollback: run the **ci-cd** workflow by hand with an earlier release SHA, or
`make up-app VERSION=<sha>` (then `make smoke-aws VERSION=<sha>`).

The AMI is fixed per stack (ADR-0017): a new stack gets the current Amazon Linux 2023
image, updates keep it. Upgrading replaces every instance and the replicas' data:
`make up-app IMAGE_ID=ami-...`, only on purpose.

## Verify it

| Check | How |
| --- | --- |
| Code and contract | `make test` (CI runs it on every push) |
| Templates | `make lint`; `make verify` (no internet route in private subnets); `make drift` |
| Deployed app | `make smoke-aws`: the coordinator and all 6 replicas report the release that was meant to be deployed, then all four operations; `make smoke-replica`: the same with one follower of each shard stopped |
| Replication and failures | `scripts/failure-tests.sh` on compose, output in [`docs/m3/evidence/local-docker.txt`](docs/m3/evidence/local-docker.txt); in AWS [`docs/m3/evidence/aws-deploy.txt`](docs/m3/evidence/aws-deploy.txt) (follower down, catch-up). Log records, hash chain, truncation: `go test ./internal/store/` |
| M2 behaviour on failure | [`docs/m2/evidence/local-docker.txt`](docs/m2/evidence/local-docker.txt): shard restart replays its log; a stopped shard's keys answer 503, the rest keep working; coordinator restart waits for the shards (`/health` is 503 until then). Damaged logs and failed writes: `go test ./internal/store/` (ADR-0018) |
| Performance | `make up-app BENCH=true && make bench-aws && make down-app`, or the **benchmark** workflow; variants: `make up-app BENCH=true W=1` then `make bench-aws WORKLOADS=a LABEL=w1` |

The scenarios each check must satisfy are in the
[M3 specs](openspec/changes/m3-replication/specs/) and the
[M2 specs](openspec/changes/m2-sharding/specs/).

## What changed since M2

- **Replication:** every shard is a replica set of 3, one replica per AZ; 3 shard nodes each
  run one replica of each shard (two containers, ports 8081 and 8082). Single leader per
  shard, followers pull its log (ADR-0020, 0023).
- **New log record format:** a format-version byte, epoch, LSN, hash link and a checksum
  over the whole record (ADR-0022). M3 doesn't read M2 logs (it reports an unknown format
  version); M3 stacks start empty.
- **Coordinator:** membership per replica, leader and epoch persisted, automatic promotion,
  writes to the leader, quorum reads (ADR-0020, 0021).
- **API:** public API unchanged except a new **504** on writes ("not committed, outcome
  unknown"); `/health` reports 503/504 counts. Internal API: replica heartbeats, the log
  endpoint, version headers ([`api/internal-openapi.yaml`](api/internal-openapi.yaml)).
- **Infrastructure:** `infra/app.yaml` has NodeA/B/C instead of Shard1/Shard2 and a `W`
  parameter; the coordinator's two ingress rules are separate resources and the client
  range is optional.
- **Pipeline:** a second smoke step stops a follower-only node (`make smoke-replica`).
- **Results:** workload A 1,545 ops/s with W = 2 (−14% vs M2), 1,728 with W = 1; one
  follower per shard down costs ~1% and no errors ([`docs/m3/benchmark.md`](docs/m3/benchmark.md)).

## What changed in M2 (since M1)

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
