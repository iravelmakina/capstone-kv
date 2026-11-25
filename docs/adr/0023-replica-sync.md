# ADR-0023: Replica synchronisation and rejoin

- Status: **Accepted** — 2026-11-25 by Iryna Velmakina (author). Drafted with Claude Code from
  the decision list agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).
- Date: 2026-11-25

## Context

Followers must get the leader's records in order, resync only the missing range after a
restart, and, with W = 2 (ADR-0021), one follower must durably ack a write before it
commits. Leaders can change (ADR-0020), so a returning replica may hold records the new
leader never had. Records carry `(epoch, LSN)` and a hash chain (ADR-0022).

## Options

| | 1. Pull: followers ask for records after their position | 2. Push: the leader sends each record |
| --- | --- | --- |
| Restart / catch-up | the same call as normal operation | a separate catch-up path |
| Leader keeps per follower | the last acked LSN | a send queue and retries |
| Ack for W | the next pull's position *is* the ack | a reply to each push |

## Decision

**Option 1, pull with long-polling.**

1. **Pull.** A follower calls `GET /internal/v1/log?epoch=&lsn=&hash=&max=` with the epoch,
   LSN and hash of its last durable record. The current leader answers at once with up to
   `max` records after it, or holds the request until one appears (5 s, then empty). A
   replica that isn't the current epoch's leader answers 409.
2. **Ack.** The follower appends, fsyncs and applies the records, checking each checksum and
   hash link, then pulls again at once. That next pull's position is its ack: the leader
   records it as the follower's durable LSN.
3. **Commit.** The leader appends and fsyncs a write, waits until one follower's durable
   LSN reaches it, then **applies it to its own map** and answers 200. So a read from the
   leader never returns a write that isn't on two disks. Followers apply what they receive:
   once a follower has a record, it's on two disks. No follower within **2 s** → the leader
   answers "not committed" and the coordinator returns **504 outcome unknown** (ADR-0021).
4. **Restart.** A follower replays its own log, then pulls from its last record: only the
   missing range moves.
5. **Rejoin after a leader change (truncation).** If the follower's last record isn't in the
   leader's log (its hash doesn't match the leader's record at that LSN, or it's past the
   leader's end), the leader answers with the **last LSN of the follower's epoch** in its own
   log. The follower cuts its log back to that LSN and pulls again. This is how a former
   leader drops writes that never committed (the same idea as Kafka's leader epochs).
6. **Damaged replica.** A replica whose log is damaged in the middle (ADR-0018 refuses to
   start it) is wiped and resynced from the leader by a person (`make rebuild-replica`).

**Timeouts, together:**

| What | Value |
| --- | --- |
| Heartbeat | every 5 s |
| Replica unavailable / leader dead | 3 missed = 15 s |
| Long-poll held by the leader | 5 s |
| Follower's deadline on a pull | 7 s, then it retries against the leader the coordinator names |
| Commit wait | 2 s → 504 |
| Coordinator asking followers before a promotion | 1 s each |

**Metrics** on each replica's health endpoint: role, epoch, last LSN, lag behind the leader,
commit-wait time (last and 99th percentile since start). The coordinator counts 503s and
504s per shard. These are M4's monitoring inputs.

**Runner-up:** push. Reconsider if pull latency dominates write p99.

## Accepted downsides

- A 504 write may appear later; the client can't tell "lost" from "late".
- Every write pays a long-poll return plus a follower fsync before it commits.
- A follower far behind reads a long range from the leader's disk.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Both followers down: every write times out | Medium | 504 within 2 s; reads need 2 live replicas anyway |
| Truncation removes a committed record | High | Only records past the new leader's log are cut; the new leader holds every committed write (ADR-0020's promotion rule) |

## Related

- Implemented in: `internal/shard/` (log endpoint, pull loop, commit wait, truncation), `api/internal-openapi.yaml`.
- Uses: ADR-0020, ADR-0021, ADR-0022.
