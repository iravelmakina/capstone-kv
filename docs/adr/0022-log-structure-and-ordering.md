# ADR-0022: Log structure and ordering guarantees

- Status: **Accepted** — 2026-11-25 by Iryna Velmakina (author). Drafted with Claude Code from
  the decision list agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).
- Date: 2026-11-25
- Supersedes: ADR-0013's log record format (records now carry a format version, epoch, LSN
  and hash). Settles ADR-0018's deferrals: the header checksum and re-sync of a damaged
  replica are done here; group commit moves to M5 (below). The rest of both ADRs stands.

## Context

The leader must fan its writes out to followers through a log, and the brief asks to
"think about delivery order and versions". Each shard already writes an append-only log
with a length and checksum per record, fsync per write, and replay at start (ADR-0013,
ADR-0018). Leaders can change (ADR-0020), so a version must stay unique across leaders.
The lab denies MSK, Amazon MQ, SQS and Kinesis; instances can't write S3 without a bucket policy.

## Options

| | 1. The leader's own log | 2. Log segments in S3 | 3. Kafka or RabbitMQ on our EC2 |
| --- | --- | --- | --- |
| New moving parts | none | bucket policy, segment naming, polling S3 | a broker cluster to run, patch and pay for |
| Write latency | one local fsync | an S3 PUT per write or per batch | a network hop to the broker |
| Ordering | one writer per shard | one writer per shard | per partition |
| Explainable "without notes" | yes, it's our code | yes | mostly the broker's internals |

## Decision

**Option 1.** The leader's log becomes the replication log. A record is:

```
format version (1 B) | length (4 B) | epoch (8 B) | LSN (8 B) | prev hash (8 B) |
op (1 B: PUT or DELETE) | key length (2 B) | key | value | checksum (4 B, over everything before it)
```

- **LSN:** a counter per replica set, assigned by the leader, +1 per record. **Epoch:** the
  leader epoch it was written in (ADR-0020). A key's **version** is `(epoch, LSN)` of its
  last write, compared epoch first; it stays unique across leader changes.
- **Ordering:** total within a shard (one leader per epoch assigns LSNs); none across
  shards, and none is needed: a key lives in exactly one shard.
- **Tombstones:** a `DELETE` is kept, with its version, in memory and in the log, so a read
  can tell "deleted at (2, 90)" from "old value at (1, 40)".
- **Hash chain:** `prev hash` is `xxhash64` of the previous record.
  A follower checks it on every record it receives, so two replicas that disagree about
  any earlier record are detected at the next record, not by a later scan. This is our
  answer to the brief's optional anti-entropy task, without a Merkle tree.
- **Checksum** covers the whole record, including length, epoch and LSN.
- **Format version** lets M4 add fields (e.g. a timestamp for conflict resolution)
  without breaking existing logs.
- **Followers** write the same records to their own log and apply them in LSN order; a
  record at or below their last LSN is skipped. Their position is the last record in their
  log, so no separate offset file.
- **No compaction** in M3.

**Not now:** a hybrid logical clock per record (M4 decides how conflicts are resolved; the
format version leaves room), and **group commit**, promised in ADR-0018: M3's benchmark
measures what replication costs against M2, and batching fsyncs at the same time would
change two things at once. It moves to M5, measured on its own.

**Runner-up:** option 2. Reconsider if followers must catch up while every leader is down.

## Accepted downsides

- 36 bytes of fixed overhead per record.
- The log never shrinks; replay and resync read it from the start.
- Tombstones stay in memory forever in M3.
- One fsync per write until M5.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| M2 logs can't be read | Low | M2 data is test data; M3 starts from empty volumes |
| Two replicas hold different records with the same LSN | High | Epoch in the version; hash chain detects it; rejoin truncates (ADR-0023) |

## Related

- Implemented in: `internal/store/` (record format, versions, tombstones, hash chain, reading a range).
- Uses: ADR-0018 (damage handling). Used by: ADR-0021, ADR-0023.
