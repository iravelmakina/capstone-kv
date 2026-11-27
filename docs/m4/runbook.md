# Runbook: capstone-kv alerts

> Written by Claude Code for M4 (task 9.1); the author reviews it. One section per alert in
> `monitoring/rules.yml`. Where to look: Grafana (dashboard "capstone-kv"), Loki through
> Grafana's Explore view, `GET /internal/v1/shards` on the coordinator.

## Getting there

| | Locally (docker compose) | In AWS |
| --- | --- | --- |
| Grafana | http://localhost:3000 | `make grafana` (SSM port forwarding), then http://localhost:3000 |
| Prometheus, Alertmanager | http://localhost:9090, :9093 | `make prometheus` (same way) |
| Replica sets | `curl localhost:8080/internal/v1/shards` | on the coordinator through SSM, or from the laptop if its IP is allowed |
| Logs of one request | dashboard panel "Logs of one request", type the request ID | the same |
| Telegram | the chat the bot writes to (`secrets/telegram/`) | the same bot, from `velmakina-kv/monitoring` |

Detection time for a node stop, as designed: 15 s of missed heartbeats, ≤ 5 s to the next
scrape, ≤ 5 s to the next rule evaluation, 5 s `group_wait`: about 30 s, plus delivery.
Measured values are in `docs/m4/evidence/` (task 8.2).

## CoordinatorDown (critical)

**Means:** the probe from the monitor fails, or the coordinator's metrics port doesn't
answer. Every request fails: the coordinator is the only entry point (ADR-0020).
**Check:** `docker ps` on the coordinator (SSM), its logs (`docker logs kv-coordinator`);
whether the API key file loaded ("load API keys" at start: the coordinator refuses to
start without it). A 401 from the probe means the probe key is wrong, not that the store is
down.
**Do:** `docker start kv-coordinator` (it restarts by itself after a crash). If the keys
file is missing: `make rotate-keys`, then start it. Leaders and epochs survive a restart
(they are in its own log).

## NodeDown (critical)

**Means:** every replica on one node is silent: the instance, or Docker on it, is down. That
is one replica of each shard (ADR-0020); reads and writes keep working on the other two.
**Check:** the instance's state (`aws ec2 describe-instances`); "Replicas up" and "Leader
epoch" on the dashboard: if one of its replicas was a leader, a promotion follows within
about 20 s.
**Do:** start the instance (`aws ec2 start-instances`). Its replicas pull what they missed
and catch up on their own; watch "Replication lag" go back to 0. A second node down before
then makes the shards read-only or unavailable: don't stop two.

## ReplicaDown (warning)

**Means:** one replica is silent while others on its node are up: its container stopped or
is restarting.
**Check:** `docker ps -a` on the node; the replica's last log lines in Loki
(`{container="kv-1b"}`). A replica whose log is damaged in the middle refuses to start
(ADR-0018): its log says so.
**Do:** a crash restarts by itself. A damaged log: rebuild the replica from its leader
(`make rebuild-replica` locally; in AWS remove its data folder and restart the container).

## ShardWithoutLeader (critical)

**Means:** a shard's leader is down and no follower has been promoted for 30 s. Promotion
needs both followers to answer (ADR-0020), so one follower is also down or unreachable.
Writes to that shard get 503; reads too if fewer than 2 replicas are live.
**Check:** `GET /internal/v1/shards`: which replicas are available, their last LSNs.
**Do:** bring back any replica of that shard: with two live replicas and the old leader
silent, the coordinator promotes the more up-to-date one. Never force a promotion by hand:
that is how committed writes get lost.

## ScrapeFailing (warning)

**Means:** Prometheus can't reach a replica's metrics port, though the coordinator may still
see it.
**Check:** the target's state in Prometheus (Status → Targets); the security group allows
the monitor on 9181/9182 only.
**Do:** fix the target or the security group; the replica's own health is in ReplicaDown.

## MetricsMissing (critical)

**Means:** the coordinator answers but reports no replicas at all: none has sent a
heartbeat since it started.
**Check:** the replicas' logs for "heartbeat to … failed"; the coordinator's address they
use (`KV_COORDINATOR_URL`); the security groups between them.
**Do:** fix the address or the network; the replicas report within 5 s.

## ReadLatencyHigh / WriteLatencyHigh (warning)

**Means:** a shard's read p99 above 15 ms (write above 60 ms) for 5 minutes, with at least
one request a second.
**Check:** "Replica latency p99": one slow replica or all of them? "Commit wait p99" for
writes: a slow follower fsync. CPU credits of the nodes (EC2 metrics): a t3 at 0 credits
without `unlimited` would throttle (ours are `unlimited`). A follower down makes reads pair
the other two (no rotation).
**Do:** one slow replica: restart its container or move it; all slow: the load is above
what the benchmark measured (ADR-0026), which is M5's autoscaling question.

## ReplicationLagHigh (warning)

**Means:** a follower hasn't held its leader's last record for over 10 s, measured on the
leader.
**Check:** is the follower up (ReplicaDown)? Its log for pull errors ("pull from … status
409/5xx", "broken chain", "cutting back").
**Do:** a stopped follower catches up when started. A follower that keeps failing on a
broken hash chain needs a rebuild from the leader.

## WritesNotCommitted (warning)

**Means:** a shard answers writes with 504: the leader stores them but no follower confirms
within 2 s. Their outcome is unknown, not failed (ADR-0021).
**Check:** both followers of that shard: down, lagging, or cut off from the leader.
**Do:** bring a follower back; the timed-out writes commit when it pulls them. Clients that
retry must send back the stamp from the 504 (`X-KV-Timestamp`), so a retry can't overwrite
a newer write (ADR-0028).

## ErrorBudgetFastBurn (critical) / ErrorBudgetSpent (warning)

**Means:** more than 10% of requests fail (5xx) over both 5 minutes and 1 minute: at that
rate the hour's 1% budget is gone in about 6 minutes. "Spent": over 1% failed in the last
hour. Both fire only at ≥ 1 request/s; at idle, look at the cause alerts.
**Check:** "Errors by status": 503 (no leader, too few replicas) or 504 (commits timing
out)? Then the matching alert above.
**Do:** as for that alert. Losing a node that holds both leaders cost ≈ 0.93% of an hour
of workload A in the AWS drill (ADR-0030): one per hour fits the SLO, a second breaks it.
Check `kv_shard_leader_epoch`: if both leaders sit on one node, that node is the risk.

## Watchdog (info)

**Means:** nothing is wrong: it always fires, and its hourly Telegram message shows the
monitor itself is alive.
**If the message stops:** the monitor is down and nothing is being watched. Check the
monitor instance (`aws ec2 describe-instances`, SSM, `docker ps`); its containers restart
by themselves. Until it is back, watch the store by hand: `/health` on the coordinator and
`GET /internal/v1/shards`.

## Following one request

1. Take its `X-Request-ID` from the response (every response has one).
2. Dashboard → "Logs of one request" → type the ID, or in Explore:
   `{container=~".+"} | json | request_id="<id>"`.
3. The coordinator's line names the shard, status, duration and (for reads) each replica's
   time; the leader's line names the LSN and commit wait.
4. Followers by LSN: `{container=~".+"} | json | msg="appended"` and find the range that
   holds that LSN.

## Checking a release in a lab session

What was fixed after the M4 lab sessions (smoke-replica under load, monitor data across a
restart, ADR-0030's alert rules, ADR-0031's buckets, `make grafana-check`) is checked in
AWS by `scripts/m4-lab-check.sh`, about an hour in all:

1. Start the lab; put its credentials in `~/.aws/credentials` as profile `academy`, adding
   the header when the copied text has none:
   `{ pbpaste | grep -q '^\[' || echo '[academy]'; pbpaste | sed 's/^\[default\]/[academy]/'; } > ~/.aws/credentials && chmod 600 ~/.aws/credentials`
2. `make release` (Docker running; builds and uploads the current commit).
3. `CONFIRM=yes make up-app MONITORING=true BENCH=true` (~5 minutes).
4. `bash scripts/m4-lab-check.sh` (~45 minutes): smoke, `grafana-check`, the rules and
   buckets Prometheus has, a follower node stopped under workload A, one clean benchmark
   run with p99 compared to the client's, the monitor stopped and started. It prints
   PASS / FAIL per check and saves everything to `bench-results/<version>/`.
5. `make down-app`.
