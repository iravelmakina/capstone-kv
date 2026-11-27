# Tasks

> Group 1 is the spec gate: no code or IaC until 1.4 is done.

## 1. Spec gate

- [x] 1.1 AI design review of the M4 spec and ADRs 0025–0029 from a fresh session; transcript and verdicts in `docs/m4/ai-review.md`; ADRs, design and specs revised per the verdicts
- [x] 1.2 Author confirms the verdicts, reviews proposal, design, specs and ADRs 0025–0029, and sets them to `Accepted` in chat
- [x] 1.3 `openspec validate m4-monitoring --strict` passes
- [x] 1.4 Spec-only commit (no code, no templates); push `m4-monitoring`

## 2. Metrics and logs in the code

- [x] 2.1 `internal/metrics`: counters, gauges and fixed-bucket histograms in the Prometheus text format, Go runtime metrics (standard library only)
- [x] 2.2 Metrics on a separate listener: coordinator 9180, replicas on their API port + 1100 (9181/9182)
- [x] 2.3 Coordinator: `kv_requests_total{op,code,shard,principal}`, `kv_request_duration_seconds{op,shard}`, replica (with `node`) and leader gauges, last LSNs
- [x] 2.4 Replica: request histogram and errors by operation, role, keys, log bytes, reads; leader: commit wait, each follower's acked LSN and lag seconds
- [x] 2.5 JSON logs (`log/slog`): `X-Request-ID` taken or made by the coordinator, passed to replicas, returned; the leader logs request ID ↔ LSN, followers the LSN ranges they append; the coordinator logs each replica's time on reads; no values or keys
- [x] 2.6 Tests: metric names and labels, histogram buckets against hand-computed values, request ID passed through, follower lag on the leader

## 3. Record versions

- [x] 3.1 Record format 2 with an HLC stamp; format 1 still read (stamp 0)
- [x] 3.2 Leader stamps with `max(now, last + 1)`; a new leader starts above its log's last stamp
- [x] 3.3 The stamp on every write answer, 504 included
- [x] 3.4 `X-KV-Timestamp` from the client: 400 if its physical part is more than 5 s ahead of the leader's physical clock; HLC receive rule
- [x] 3.5 Last-write-wins against the key's latest record in the log; the superseded answer waits for that record's commit (504 if it times out)
- [x] 3.6 `ETag` (the stamp) and `X-KV-Timestamp` on reads; `If-Match` on PUT and DELETE, `If-None-Match: *` on PUT, 412 otherwise
- [x] 3.7 Tests: stamps rise across a promotion, a late retry is superseded, superseded only after commit, a future stamp refused, `If-Match` and `If-None-Match` conflicts
- [ ] 3.8 `scripts/failure-tests.sh` still passes, plus a late-retry scenario

## 4. API keys

- [x] 4.1 `internal/auth`: keys as SHA-256 hashes with name and scopes, loaded from a file and reloaded when it changes
- [x] 4.2 `Authorization: Bearer` on every public route; 401 / 403; `/health`, metrics port and `/internal` without keys; `principal` in logs and metrics
- [ ] 4.3 Smoke test, smoke-replica, benchmark client and failure tests use an ops key; `make rotate-keys` refreshes the coordinator's file through SSM
- [x] 4.4 `api/openapi.yaml`: security scheme, 401, 403, 412, 400, the new headers; contract test passes

## 5. MCP server

- [ ] 5.1 `cmd/kvmcp` on the official MCP Go SDK, stdio: `list_tables`, `read_item`, `item_exists`, `create_item` (create only), `update_item` (`If-Match`), `delete_item`, with input schemas
- [ ] 5.2 Annotations (read-only, destructive); outcomes named (created, updated, deleted, superseded, conflict, not found, not permitted, invalid, unavailable, outcome unknown); values as data, at most 16 KB
- [ ] 5.3 Tests against a test coordinator: each tool, each outcome, a missing scope, a truncated value
- [ ] 5.4 Verified with Claude Code as the MCP client (local cluster), by the author; transcript in `docs/m4/evidence/mcp-demo.md`

## 6. Local monitoring

- [ ] 6.1 `monitoring/`: Prometheus config and alert rules with `promtool` tests, Alertmanager config (Telegram, timings), blackbox exporter config, Grafana provisioning and the dashboard as JSON, Loki and Alloy configs
- [ ] 6.2 `docker-compose.yml` runs the stack next to the cluster; secrets from a git-ignored file
- [ ] 6.3 `promtool check config`, `promtool test rules`, `amtool check-config` pass
- [ ] 6.4 Local drills: a follower stopped (`NodeDown` fires and resolves), the coordinator stopped (`CoordinatorDown`), one request ID followed in Loki to the followers; output in `docs/m4/evidence/local-docker.txt`

## 7. Infrastructure

- [ ] 7.1 `infra/app.yaml`: `Monitoring` parameter, monitor instance and security group (no public ingress; Loki from the VPC groups); coordinator and shard groups admit the monitor on metrics ports only; `cfn-lint` passes
- [ ] 7.2 Alloy image delivered to private nodes through the artifacts bucket; node boot starts it
- [ ] 7.3 Secrets `velmakina-kv/api-keys`, `velmakina-kv/ops-key` (ops and probe keys) and `velmakina-kv/monitoring` (Telegram bot, chat ID, Grafana admin password) created by the author; the pipeline passes no secret values

## 8. AWS session

- [ ] 8.1 Deploy with monitoring (author approves each write call); dashboard shows all panels through SSM port forwarding; smoke tests pass with keys
- [ ] 8.2 Drills: a follower node stopped (alert time and 5xx counted), a leader node stopped (promotion on the dashboard, alerts, 5xx and read latency during the 15 s window), the coordinator container stopped (probe alert); `docs/m4/evidence/aws-deploy.txt`; ADR-0026 superseded if the drills contradict it
- [ ] 8.3 Workload A with metrics on, 3 runs, compared with the M3 control: instrumentation cost
- [ ] 8.4 Evidence exported through SSM port forwarding; `make down-app`

## 9. Review and docs

- [ ] 9.1 `docs/m4/runbook.md`: for each alert, what it means, what to check, what to do; also what to do when the monitor itself is silent
- [ ] 9.2 `docs/m4/control-questions.md` (author's notes)
- [ ] 9.3 README: monitoring, keys, MCP; what changed since M3
- [ ] 9.4 `docs/m4/cost.md`
- [ ] 9.5 `AGENT_WORKLOG.md` has a row for every agent-made file
- [ ] 9.6 After M2 and M3 are archived: MODIFIED deltas for `kv-api` and `replication`, so the archived specs don't contradict M4
