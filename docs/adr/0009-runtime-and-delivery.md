# ADR-0009: Runtime and delivery

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

M2 adds two programs, a coordinator and a shard, on EC2 (ECS and Fargate are denied, M1
evidence 08). Shards sit in private subnets with no route to the internet (ADR-0005); the
only way out is the S3 gateway endpoint, and instances may read S3 (M1 evidence 63). ECR
is denied, Docker Hub needs internet. In the 20.11 defenses the lecturer criticised a bare
artifact on EC2 and asked for containers by M3.

## Options — language

| | Go | Java / Kotlin | Python |
| --- | --- | --- | --- |
| What the node needs installed | nothing: one static binary | a JVM | an interpreter + packages |
| Image size | ~10 MB (`scratch`) | ~150 MB+ | ~100 MB+ |
| HTTP, concurrency, hashing | standard library | standard library + frameworks | frameworks |
| Benchmark tooling | go-ycsb (ADR-0016) | original YCSB | wrapper needed |

## Options — delivery

| | 1. Binary from S3 | 2. Docker image as a tarball in S3 | 3. Image from a registry |
| --- | --- | --- | --- |
| Works without internet | yes | yes, if Docker installs from Amazon Linux repos via the S3 endpoint | no: ECR denied, Docker Hub needs internet |
| Same artifact locally, in CI and on EC2 | partly | yes (`docker compose` runs the same image) | yes |
| Process supervision, isolation | systemd unit to write | `--restart always`, isolated filesystem | same |
| Lecturer's feedback | criticised | accepted | — |

## Decision

**Go, one binary with two roles** (`kvnode --role=coordinator` / `--role=shard`), **shipped
as a Docker image**: CI builds it, saves it with `docker save`, uploads it to the
artifacts bucket under the commit SHA; each instance downloads it through the S3 endpoint,
`docker load`s it and runs it with `--restart always`. Data lives on a host directory
mounted into the container.

**Runner-up:** option 1 (binary from S3). Reconsider if Docker can't be installed in a
private subnet and the switchable NAT (ADR-0005) isn't worth it.

## Accepted downsides

- A 10 MB tarball per version in S3 instead of a registry with layers.
- Docker must be installed at boot (a few seconds; one more thing that can fail).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| `dnf install docker` fails in a private subnet (repos not reachable through the S3 endpoint) | Medium | Verify on the first deploy; fallback: the switchable NAT from ADR-0005, on only during boot |
| Go garbage collection adds tail latency | Low | Measured in the benchmark (p99); tune `GOGC` only if it shows |

## Related

- Implemented in: `Dockerfile`, `cmd/kvnode/`, instance user data in `infra/app.yaml`, CI build job.
- Uses: ADR-0005 (no route out), ADR-0014 (app stack), ADR-0015 (pipeline).
