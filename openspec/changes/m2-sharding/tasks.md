# Tasks

> Group 1 is the spec gate: no code or IaC until 1.4 is done.

## 1. Spec gate and repository

- [x] 1.1 Commit the M1 archive on its own (`openspec archive m1-infra-setup`, `dc48bb6`)
- [x] 1.2 Author reviews proposal, design, specs and ADRs 0009–0016; sets them to `Accepted` in chat
- [x] 1.3 `openspec validate m2-sharding --strict` passes
- [x] 1.4 Spec-only commit (no code, no templates)
- [ ] 1.5 Author runs `gh auth login`; repo `iravelmakina/capstone-kv` created; `main` and `m2-sharding` pushed

## 2. API and ring

- [ ] 2.1 Write `api/openapi.yaml` per ADR-0010
- [ ] 2.2 `internal/ring`: xxHash64 ring with virtual nodes and stable shard IDs; unit tests for lookup, join and restart
- [ ] 2.3 Distribution test (1, 16, 64, 128, 256 virtual nodes × 2, 3, 5 shards); results in `docs/m2/ring-distribution.md`; confirm or adjust the count per ADR-0011's acceptance rule

## 3. Shard

- [ ] 3.1 `internal/store`: in-memory map + append-only log (length + checksum per record, fsync), replay at start; tests incl. a torn last record
- [ ] 3.2 `internal/shard`: internal key-value endpoints, `/health`, registration + heartbeat loop

## 4. Coordinator

- [ ] 4.1 Table registry (persisted like the store); `POST /v1/tables`
- [ ] 4.2 Item endpoints: route by table + partition key, forward to the shard; 400/404/409/503 per the spec
- [ ] 4.3 Membership: register, heartbeat, unavailable after 3 misses, explicit removal, 503 until the expected shard count is present
- [ ] 4.4 Contract test against `api/openapi.yaml`

## 5. Local run

- [ ] 5.1 `Dockerfile` (static Go build, minimal image); `docker-compose.yml` with a coordinator and 2 shards
- [ ] 5.2 End-to-end test on compose: create table, put, read, exists, delete; restart a shard and read back

## 6. Infrastructure

- [ ] 6.1 `infra/artifacts.yaml` (bucket, `Retain`) and `infra/app.yaml` per ADR-0014; `cfn-lint` passes; no `vpc-`/`subnet-` literals
- [ ] 6.2 `Makefile`: targets for the new stacks (`up-app`, `down-app`, `push-image`), teardown order app → network
- [ ] 6.3 First deploy (author approves each write call): every node healthy; record whether `dnf install docker` worked through the S3 endpoint (evidence in `docs/m2/evidence/`)
- [ ] 6.4 Check the security groups and that shards have no public IP; save the outputs

## 7. CI/CD

- [ ] 7.1 Author adds the three lab-credential secrets to the repo
- [ ] 7.2 `.github/workflows/ci-cd.yml`: test → build → deploy-infra → deploy-app → smoke
- [ ] 7.3 A push to `main` deploys a code change; a template change shows its change set; a rollback to an earlier SHA works; links to the runs in `docs/m2/pipeline.md`
- [ ] 7.4 `.github/workflows/benchmark.yml` (manual)

## 8. Benchmark

- [ ] 8.1 `bench/`: go-ycsb binding for our API
- [ ] 8.2 Workloads A and B, 3 runs each, per ADR-0016; results in `docs/m2/benchmark.md`, raw output in S3

## 9. Review and docs

- [ ] 9.1 AI design review from a fresh session; transcript and verdicts in `docs/m2/ai-review.md`
- [ ] 9.2 `docs/m2/control-questions.md` (incl. virtual nodes on node failure, load balancing and service discovery, p95 vs p99)
- [ ] 9.3 README: bring up, verify, what changed since M1
- [ ] 9.4 Actual spend for the M2 sessions vs the M1 budget
- [ ] 9.5 `AGENT_WORKLOG.md` row for every agent-made file
