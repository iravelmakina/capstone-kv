# Design

> **Approved** by the author on 2026-11-24. Drafted with Claude Code from the decision list
> agreed in chat.

## Context

- M1 delivered the network (`velmakina-kv-network`, 11 exports), the lab constraints
  (t3.small/medium, no ECS/ECR, no AWS CI/CD, no OIDC, instance role `Inventory-App-Role`
  can read S3) and a $60 course budget. Facts: `docs/m1/`.
- Private subnets have no route out; the S3 gateway endpoint is the only exit (ADR-0005).
- Lecturer's feedback from the defenses: containers rather than bare artifacts; the stack
  should name the deployed version; benchmark in its own CI job; partition key chosen by
  access pattern first.

## Goals / Non-Goals

**Goals:** the four operations across 2 shards; a restart loses no data; one push to
`main` builds, deploys and smoke-tests; a reproducible YCSB baseline.

**Non-Goals:** replication (M3), metrics and alerting (M4), autoscaling and data migration
on ring changes (M5), range queries by sort key, authentication.

## Decisions

1. **Go, one binary with two roles, shipped as a Docker image via S3.** ADR-0009.
2. **REST + JSON, OpenAPI 3.1, `/v1`.** ADR-0010.
3. **xxHash64, 128 virtual nodes, confirmed by a distribution test.** ADR-0011.
4. **Shard = ring(table + partition key); sort key not hashed.** ADR-0012.
5. **In-memory map + append-only log; register + heartbeat; a silent shard stays on the
   ring and its keys return 503.** ADR-0013.
6. **Instances in CloudFormation: coordinator public, 2 shards private in two AZs;
   artifacts in their own stack.** ADR-0014.
7. **GitHub Actions with lab keys as secrets; change sets; SSM smoke test; benchmark as a
   separate manual workflow.** ADR-0015.
8. **go-ycsb, workloads A and B, 3 runs each, median.** ADR-0016.

## Request path

```
client ──HTTP──▶ coordinator (public subnet, :8080)
                   │  table registry check
                   │  shard = ring(xxhash64(table + 0x00 + pk))
                   ▼
                 shard-N (private subnet, :8080, accepts only the coordinator's SG)
                   │  key = table / pk [/ sk]
                   ▼
                 in-memory map + append-only log on EBS
```

## Code layout

```
cmd/kvnode/          main: --role=coordinator | --role=shard
internal/api/        HTTP handlers for the public API
internal/ring/       consistent hashing ring
internal/coordinator/ table registry, routing, membership
internal/shard/      shard HTTP handlers, registration loop
internal/store/      in-memory map + append-only log
api/openapi.yaml     the API contract
bench/               go-ycsb binding and runner
```

## Risks / Trade-offs

- [Docker can't be installed in a private subnet] → verify on the first deploy; fallback:
  the switchable NAT (ADR-0005) during boot.
- [Adding a shard remaps keys without moving data] → M2 adds shards only before loading
  data; migration is M5.
- [The coordinator is a single point of failure and a bottleneck] → accepted in M2; it is
  stateless apart from the table registry, so it can be scaled out behind a load balancer
  later (control question).
- [Lab credentials expire] → deploy jobs fail outside a session; tests and builds still run.
- [Budget] → app stack up only during sessions; `make down-app` at the end.

## Migration Plan

Additive: the network stack is unchanged; new stacks are deployed next to it. Rollback is
`make down-app` or a deploy of an earlier `AppVersion`.

## Open Questions

- Whether `dnf install docker` works through the S3 endpoint (first deploy answers it).
