# Tasks

> Group 1 is the spec gate: no code or IaC until 1.4 is done.

## 1. Spec gate and repository

- [x] 1.1 Commit the M1 archive on its own (`openspec archive m1-infra-setup`, `17b61e8`)
- [x] 1.2 Author reviews proposal, design, specs and ADRs 0009–0016; sets them to `Accepted` in chat
- [x] 1.3 `openspec validate m2-sharding --strict` passes
- [x] 1.4 Spec-only commit (no code, no templates)
- [x] 1.5 Author runs `gh auth login`; repo `iravelmakina/capstone-kv` created; `main` and `m2-sharding` pushed

## 2. API and ring

- [x] 2.1 Write `api/openapi.yaml` per ADR-0010
- [x] 2.2 `internal/ring`: xxHash64 ring with virtual nodes and stable shard IDs; unit tests for lookup, join and restart
- [x] 2.3 Distribution test (1, 16, 64, 128, 256 virtual nodes × 2, 3, 5 shards); results in `docs/m2/ring-distribution.md`; confirm or adjust the count per ADR-0011's acceptance rule

## 3. Shard

- [x] 3.1 `internal/store`: in-memory map + append-only log (length + checksum per record, fsync), replay at start; tests incl. a torn last record
- [x] 3.2 `internal/shard`: internal key-value endpoints, `/health`, registration + heartbeat loop

## 4. Coordinator

- [x] 4.1 Table registry (persisted like the store); `POST /v1/tables`
- [x] 4.2 Item endpoints: route by table + partition key, forward to the shard; 400/404/409/503 per the spec
- [x] 4.3 Membership: register, heartbeat, unavailable after 3 misses, explicit removal, 503 until the expected shard count is present
- [x] 4.4 Contract test against `api/openapi.yaml`

## 5. Local run

- [x] 5.1 `Dockerfile` (static Go build, minimal image); `docker-compose.yml` with a coordinator and 2 shards
- [x] 5.2 End-to-end test on compose: create table, put, read, exists, delete; restart a shard and read back

## 6. Infrastructure

- [x] 6.1 `infra/artifacts.yaml` (bucket, `Retain`) and `infra/app.yaml` per ADR-0014; `cfn-lint` passes; no `vpc-`/`subnet-` literals
- [x] 6.2 `Makefile`: targets for the new stacks (`up-app`, `down-app`, `push-image`), teardown order app → network
- [x] 6.3 First deploy (author approves each write call): every node healthy; record whether `dnf install docker` worked through the S3 endpoint (evidence in `docs/m2/evidence/`)
- [x] 6.4 Check the security groups and that shards have no public IP; save the outputs

## 7. CI/CD

- [x] 7.1 Author adds the three lab-credential secrets to the repo
- [ ] 7.2 `.github/workflows/ci-cd.yml`: test → build → deploy-infra → deploy-app → smoke
- [x] 7.3 A push to `main` deploys a code change; a template change shows its change set; a rollback to an earlier SHA works; links to the runs in `docs/m2/pipeline.md`
- [x] 7.4 `.github/workflows/benchmark.yml` (manual)

## 8. Benchmark

- [x] 8.1 `bench/`: go-ycsb binding for our API
- [ ] 8.2 Workloads A and B, 3 runs each, per ADR-0016; results in `docs/m2/benchmark.md`, raw output in S3

## 9. Review and docs

- [ ] 9.1 AI design review from a fresh session; transcript and verdicts in `docs/m2/ai-review.md`
- [ ] 9.2 `docs/m2/control-questions.md` (incl. virtual nodes on node failure, load balancing and service discovery, p95 vs p99)
- [ ] 9.3 README: bring up, verify, what changed since M1
- [ ] 9.4 Actual spend for the M2 sessions vs the M1 budget
- [ ] 9.5 `AGENT_WORKLOG.md` row for every agent-made file
