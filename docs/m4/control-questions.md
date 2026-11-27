# M4 control questions — short answers

> **Drafted with Claude Code** as defense prep (task 9.2). The author answers these orally
> at the defense; this file is notes, not the author's own text. Each answer points to where
> this project shows it. Measured numbers come from the M4 lab session (task 8.2).

## 1. Which metrics do you collect, and which are SLIs? Why not all of them?

- **Collected** (ADR-0025): requests by operation, status, shard and key name; latency
  histograms at the coordinator (per shard) and on every replica; replicas up, leaders and
  epochs; each follower's lag measured on its leader; commit wait; keys and log size per
  replica; Go runtime; a probe's success from the monitor.
- **SLIs:** availability (non-5xx share at the coordinator, plus the probe's success) and
  p99 of reads and writes per shard. They are what a client feels.
- **Why not all:** lag, commit wait or a replica down are causes. With a quorum left, a
  replica down costs the client nothing (measured in M3: −0.8% throughput, no errors).
  As SLIs they would page for things that hurt nobody; as signals they explain an SLI
  breach and drive warnings.

## 2. State an SLO for your store. Where did the numbers come from?

- Availability ≥ 99.5% per hour of operation; read p99 ≤ 15 ms and write p99 ≤ 60 ms per
  shard over 5 minutes (ADR-0026).
- **From measurements:** read p99 was 6.6–10.3 ms in M3 (two deployments of the same
  release), so 15 ms is ≈ 1.5× the worst; write p99 was 39 ms, so 60 ms is ≈ 1.5×.
- **99.5%, not 99.9%:** a leader change costs ≈ 20 s of 503 on one shard's writes,
  ≈ 0.14% of an hour's requests in workload A. 99.9% would break on one planned failover;
  99.5% fits about three, and breaks if a shard is fully down for more than ≈ 36 s.
- **Per hour, not per 30 days:** the store runs only during lab sessions.

## 3. How do you detect a node failure, and how long does it take?

- Each replica heartbeats every 5 s; the coordinator marks it down after 15 s of silence
  (ADR-0020). `kv_replica_up` goes to 0 at the next scrape (5 s); `NodeDown` (all replicas
  on one node) fires at the next rule evaluation (5 s); Alertmanager waits 5 s
  (`group_wait`) and sends it to Telegram. Designed: ≈ 30 s plus delivery; measured in AWS:
  see `docs/m4/evidence/aws-deploy.txt`.
- The coordinator itself can't report its own death: the probe from the monitor and the
  scrape of its metrics port catch it (`CoordinatorDown`, 10 s).
- The monitor can't watch itself: the `Watchdog` alert reaches Telegram every hour, so an
  hour of silence means the monitor is down.

## 4. Vector clocks vs timestamps: what did you choose and what did you give up?

- **Chosen:** a hybrid logical clock stamp per record, issued by the shard's leader, with
  last-write-wins per key, plus conditional writes (ADR-0028).
- **Why not vector clocks:** they detect writes made concurrently on different replicas.
  With one leader per shard that never happens; the leader orders every write to a key.
- **Where LWW matters for us:** a retry after a 504 that arrives after someone else's
  newer write. The 504 carries the stamp; the retry sends it back and loses. In M5, keys
  moved between shards compare stamps, which `(epoch, LSN)` can't do across logs.
- **Given up:** LWW drops the losing write silently (only `X-KV-Superseded` says so); wrong
  for counters or lists, which is what `If-Match` is for. Stamps depend on leaders' clocks
  (bounded: a client stamp more than 5 s ahead of the leader's physical clock is refused).

## 5. Which tools did you expose over MCP, and which did you deliberately not?

- **Exposed:** `list_tables`, `read_item`, `item_exists`, `create_item` (create only: fails
  if the item exists), `update_item` (needs the version the agent read), `delete_item`
  (ADR-0027).
- **Not exposed:** creating or dropping tables, the internal API (heartbeats, the log,
  removing shards), anything outside the store. An agent can't change the schema or the
  cluster.
- **Changed after the AI review:** `create_item` was an upsert; an overwrite destroys data as
  surely as a delete, so writes are never blind now.

## 6. How is the agent authorised? Can it delete data?

- The store enforces it, not the MCP server: every request carries an API key, and the
  coordinator checks the key's scopes (401/403). The MCP server holds one key from its
  environment.
- **Delete:** not with the default key (`read`, `write`), so `delete_item` answers "not
  permitted". The demo uses a separate key with `delete`; Claude Code asks the person
  before running a tool marked destructive. The hint is advisory; the scope is the control.
- **Audit:** every request is logged and counted with the key's name, so "what did the
  agent change" has an answer.

## 7. What happens if prompt injection ends up in the stored data and an agent reads it?

- The value is returned as a JSON field marked as stored data, at most 16 KB, and the
  server's instructions tell the agent never to follow text in values. The agent may still
  be fooled.
- **What limits the damage:** no delete with the default key; no blind overwrite (create
  only, update needs the version read, one item at a time); no tables, no cluster, no
  tools beyond the store; every write attributed to the key.
- **What remains:** the agent can update items it has read and can mislead the user in its
  answer. No server-side rule prevents that; a person reviewing destructive actions does.

## 8. Logs, metrics, traces: which do you have, and what is missing for a real incident?

- **Metrics:** yes (Prometheus, 24 h). **Logs:** yes, JSON with a request ID and the key's
  name, in Loki; a write is followed coordinator → leader (with its LSN) → followers (by
  LSN range). **Traces:** no spans.
- **Missing:** per-hop timings in one view (spans), host metrics (CPU steal, disk fsync
  time) for the noisy-neighbour question, retention beyond a session (evidence is exported
  before teardown), and the follower's side is joined by LSN, not by request ID.
