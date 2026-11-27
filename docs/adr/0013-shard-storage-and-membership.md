# ADR-0013: Shard storage and membership

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24
- Superseded in part by ADR-0020: a silent shard's keys return 503 (now per replica set).
- Superseded in part by ADR-0022: the log record format (records get an LSN).
- Superseded in part by ADR-0033: a registering shard is no longer added to the ring at
  once; it waits as *pending* until a migration adds it.

## Context

Each shard stores its keys; M2 has no replicas, so a restart must not lose data. The brief
requires shards to register with the coordinator when they start. M3 will fan writes out
through a log; M5 will add and remove shards.

## Options — storage

| | 1. In memory only | 2. In memory + append-only log on disk | 3. Embedded engine (BoltDB, Pebble) |
| --- | --- | --- | --- |
| Survives a restart | no | yes: replay the log | yes |
| Code to explain | least | small: append, replay, fsync | a library's internals |
| Groundwork for M3 log | none | the same log format | none |
| Log grows forever | — | yes (compaction later) | engine handles it |

## Options — membership

| | A. Static list in the coordinator's config | B. Shard registers at start, then heartbeats | C. Service registry (Cloud Map, Consul) |
| --- | --- | --- | --- |
| Brief: "registered upon start" | no | yes | yes |
| Survives a coordinator restart | yes | yes: re-registered within one heartbeat | yes |
| Extra parts | none | none | Cloud Map denied (M1 evidence 62); Consul to run |

## Decision

**Storage: option 2.** A map in memory plus an append-only log on the instance's EBS volume
(`PUT` and `DELETE` records, fsync per write), replayed at start. The coordinator keeps
its table registry the same way.

**Membership: option B.** A shard calls `POST /internal/v1/shards {id, address}` at start
and every 5 s. The coordinator adds an unknown ID to the ring and marks a shard
unavailable after 3 missed heartbeats: requests for its keys get **503**, and it **stays on
the ring**. Removing a shard is an explicit call (`DELETE /internal/v1/shards/{id}`).
Without replicas, silently remapping a dead shard's keys would return "not found" for data
that still exists.

**Runner-up:** storage option 3. Reconsider if log replay becomes slow or compaction is needed.

## Accepted downsides

- The log is never compacted in M2.
- Adding a shard remaps about 1/(n+1) of the keys **without moving their data**: those
  keys read as missing until M5's migration exists. M2 adds shards only before loading data.
- fsync per write costs latency; measured in the benchmark.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Coordinator restarts and routes before shards re-register | Medium | Return 503 until the expected shard count (a parameter) has registered |
| Torn last record after a crash | Low | Length + checksum per record; replay stops at the first bad one |

## Related

- Implemented in: `internal/store/`, `internal/shard/`, `internal/coordinator/membership.go`.
- Uses: ADR-0011 (ring), ADR-0014 (EBS volume).
