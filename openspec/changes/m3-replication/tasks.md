# Tasks

> Group 1 is the spec gate: no code or IaC until 1.5 is done.

## 1. Spec gate

- [ ] 1.1 After the M2 defense: `openspec archive m2-sharding`, committed on its own
- [x] 1.2 AI design review of the M3 spec and ADRs 0020–0023 from a fresh session; transcript and verdicts in `docs/m3/ai-review.md`; ADRs, design and specs revised per the agreed verdicts
- [x] 1.3 Author reviews proposal, design, specs and ADRs 0020–0023; sets them to `Accepted` in chat
- [x] 1.4 `openspec validate m3-replication --strict` passes
- [x] 1.5 Spec-only commit (no code, no templates); push `m3-replication`

## 2. Store

- [x] 2.1 New record format (ADR-0022): format version, epoch, LSN, previous hash, checksum over the whole record; M2 damage rules unchanged
- [x] 2.2 Versions `(epoch, LSN)` and tombstones in memory; last record after replay
- [x] 2.3 Read the records after a position; truncate back to an LSN
- [x] 2.4 Tests: consecutive LSNs, tombstone survives restart, duplicate skipped, broken hash link detected, truncation, torn tail still handled

## 3. Replication on the shard

- [x] 3.1 Role and epoch from the coordinator's heartbeat answer; 409 for writes and pulls when not the current leader
- [x] 3.2 Leader: `GET /internal/v1/log` with a 5 s long-poll; follower's durable LSN from its pull position; answer "cut back to LSN n" when the follower's last record isn't in the log
- [x] 3.3 Follower: pull loop with a 7 s deadline, verify checksum and hash link, append + fsync, apply in order, truncate when told; resync from its last record after a restart
- [x] 3.4 Leader: commit wait for one follower (W = 2) or none (W = 1), apply after commit, "not committed" after 2 s
- [x] 3.5 Reads return the value with its version, or a tombstone with its version
- [x] 3.6 Health endpoint: role, epoch, last LSN, lag, commit-wait time
- [ ] 3.7 `make rebuild-replica`: wipe a replica's data and let it resync (built with the compose file, group 5)
- [x] 3.8 `api/internal-openapi.yaml` updated; internal contract test passes

## 4. Coordinator

- [x] 4.1 Membership per replica; leader and epoch per shard persisted in the coordinator's log; serve a shard once its leader is known and 2 of 3 replicas are live
- [x] 4.2 Writes to the leader only; 503 without a leader; 504 when the leader reports "not committed"
- [x] 4.3 Reads and exists: R rotating live replicas in parallel, newest `(epoch, LSN)` wins, 503 below R live
- [x] 4.4 Automatic promotion: after 3 missed heartbeats ask both followers (1 s each), promote the higher `(epoch, LSN)` under epoch + 1, persist, announce; none if a follower doesn't answer
- [x] 4.5 W/R setting (`W=2,R=2` default, `W=1,R=3`); 503/504 counts on the health endpoint
- [x] 4.6 `api/openapi.yaml`: 504 on writes; public contract test passes

## 5. Local run

- [ ] 5.1 `docker-compose.yml`: coordinator + 3 nodes × 2 replicas
- [ ] 5.2 Failure tests on compose, one per scenario of the `replication` and `quorum-reads` specs (follower down, both followers down, leader replaced, only one follower live, former leader returns, follower restart, broken hash link, rebuild, coordinator restart, balanced reads); output in `docs/m3/evidence/local-docker.txt`

## 6. Infrastructure

- [ ] 6.1 `infra/app.yaml`: 3 shard instances in the 3 AZs, two replica containers each on different ports; shard security group allows its own members and the coordinator; `cfn-lint` passes
- [ ] 6.2 First M3 deploy (author approves each write call): all 5 instances healthy; placement and security groups checked; `make down-app` afterwards
- [ ] 6.3 Evidence in `docs/m3/evidence/aws-deploy.txt`

## 7. CI/CD

- [ ] 7.1 Smoke test also checks that a put is readable with one follower stopped
- [ ] 7.2 A deploy from `main` goes green with the M3 stack

## 8. Benchmark

- [ ] 8.1 Workloads A and B with W = 2; workload A with W = 1; workload A with one follower per shard stopped; 3 runs each, settings of ADR-0016
- [ ] 8.2 `docs/m3/benchmark.md`: M2 vs M3 table, the two variants, `CPUCreditBalance`, commit SHA, why p99 changed

## 9. Review and docs

- [ ] 9.1 `docs/m3/control-questions.md` (author's notes), using the design's failure-mode table for the CP/AP and staleness questions
- [ ] 9.2 README: how to run with replicas; what changed since M2
- [ ] 9.3 `docs/m3/cost.md`: spend of the M3 sessions, including any CPU-credit surplus, against the remaining credit
- [ ] 9.4 `AGENT_WORKLOG.md` has a row for every agent-made file
