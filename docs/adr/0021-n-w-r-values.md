# ADR-0021: Values of N, W and R

- Status: **Accepted** — 2026-11-25 by Iryna Velmakina (author). Drafted with Claude Code from
  the decision list agreed in chat; revised after the AI design review (`docs/m3/ai-review.md`).
- Date: 2026-11-25

## Context

The brief asks for quorum reads with **R + W > N**: N replicas hold a key, a write commits
after W of them have durably acked it, and a read asks R of them and returns the newest
version. When R + W > N, every read set overlaps every write set in at least one replica,
so a read always sees the latest committed write. The brief adds "W = 1 for
single-leader". Reads must also spread over all replicas, not just the leader.

## Options (N = 3, single leader)

| | W=1, R=3 | **W=2, R=2** | W=3, R=1 | W=1, R=1 |
| --- | --- | --- | --- | --- |
| R + W > N | yes | yes | yes | **no** (stale reads) |
| A write waits for | the leader's fsync | leader + 1 follower | all 3 | the leader |
| Write survives losing | nothing until replicated | any 1 replica | any 2 | nothing until replicated |
| A read waits for | all 3 (slowest of 3) | 2 (slower of 2) | 1 | 1 |
| Reads with 1 replica down | **fail** | work | work | work |
| Writes with 1 follower down | work | work | **fail** | work |

## Decision

**N = 3, W = 2, R = 2 by default.** A write commits when the leader and at least one
follower have it on disk; a read asks two replicas and returns the one with the higher
version `(epoch, LSN)` (ADR-0022). Both keep working with any one replica down; writes also
need a leader (ADR-0020).

This departs from the brief's "W = 1 for single-leader" on purpose. With W = 1 and N = 3,
R + W > N forces **R = 3**: every read waits for the slowest replica and fails when one is
down, which defeats the reason for replicas. W = 2 costs one extra round trip on writes
and buys reads that survive a lost replica and a committed write that survives losing the
leader's disk.

**W is a setting** of the coordinator and the leaders: `W=2, R=2` (default) or `W=1, R=3`.
The benchmark runs both, so the cost of synchronous vs asynchronous follower acks is
measured, not argued.

The coordinator picks the 2 replicas for each read by rotating through the live ones, so
read load spreads evenly over all 3 (each replica gets about 2/3 of the shard's reads).

**When a commit times out** (no follower within 2 s, ADR-0023) the write's outcome is
**unknown**: it's on the leader's disk and may still replicate. The coordinator answers
**504** "outcome unknown", distinct from 503 "not accepted", and the public API documents
both. A client that retries may overwrite a newer write from someone else; M4 deals with
competing writers.

**Runner-up:** W=1, R=3 as the default. Reconsider if write p99 matters more than read
availability.

## Accepted downsides

- Writes are slower than W = 1: the leader waits for a follower's fsync.
- A read waits for the slower of two replicas, so read p99 rises too.
- Three replicas give about 1.5× one replica's read capacity, because every read costs two.
- With two replicas of a shard down, its reads return 503.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A slow replica drags every read it's picked for | Medium | Measured, including a run with a follower stopped; hedged reads are an M5 option |

## Related

- Implemented in: `internal/coordinator/` (read fan-out, newest version, 504), `internal/shard/` (commit wait).
- Uses: ADR-0022 (versions), ADR-0023 (acks).
