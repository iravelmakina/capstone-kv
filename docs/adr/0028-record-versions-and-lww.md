# ADR-0028: Record versions and last-write-wins

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("я все приймаю"),
  together with the AI-review verdicts. Drafted with Claude Code from the decision list she
  approved; revised after the AI design review (`docs/m4/ai-review.md`).
- Date: 2026-11-27
- Extends: ADR-0022 (record format; its format-version byte was kept for this). Settles the
  M3 risk in ADR-0021 that a retried write can overwrite a newer one.

## Context

The brief asks for record versions (vector clocks or timestamps) and last-write-wins
(LWW). With one leader per shard, writes to a key already have one order, `(epoch, LSN)`
(ADR-0022), so two replicas never hold conflicting versions they can't order. Conflicts
still reach us in two ways:

- **A late retry.** A write gets 504 "outcome unknown" (ADR-0021); the client retries; in
  between another client wrote the key. The retry arrives last and overwrites the newer
  value, though it was made earlier. This is an idempotency problem as much as a clock one.
- **Across shards (M5).** When rebalancing moves a key, the old and the new shard both hold
  a version of it. Their `(epoch, LSN)` come from different logs and can't be compared.

## Options

| | 1. `(epoch, LSN)` + `If-Match` only | 2. Vector clocks | 3. HLC stamp issued by the leader, returned to the client | 4. HLC stamp from the client's clock |
| --- | --- | --- | --- | --- |
| Orders writes within a shard | yes | yes | yes | yes |
| A late retry loses to a newer write | no | only with the client in the vector | yes: the retry sends back the stamp it got | yes, if the client stamped the first attempt |
| Depends on client clocks | no | no | no | yes: a slow clock always loses |
| Comparable across shards (M5) | no | yes, but needs merging | yes (with the shard ID as tie-breaker) | yes |
| Detects concurrent writes | no need with one leader | yes, its purpose | no: picks one | no |
| Size per record | 0 | grows with writers | 8 bytes | 8 bytes |

## Decision

**Option 3, with LWW at the leader and conditional writes as the client-side strategy.**

- **Record format 2** adds an 8-byte HLC stamp: physical milliseconds (48 bits) and a
  logical counter (16 bits). Format 1 records are still read, with stamp 0.
- **The leader stamps** each write with `max(now, last stamp + 1)`. A new leader starts
  above the last stamp in its log, so stamps rise in log order across promotions too, and
  are unique within a shard.
- **Every write answer carries the stamp** (`X-KV-Timestamp`), the 504 included: the leader
  has appended the record before its commit wait times out, so it knows the stamp.
- **A retry sends that stamp back** in `X-KV-Timestamp`. A stamp whose physical part is
  more than 5 s ahead of the leader's **physical** clock gets 400 (the bound doesn't move
  with the HLC, so it can't ratchet); the leader moves its HLC forward to an accepted stamp.
- **LWW:** a write whose stamp isn't higher than the key's latest record (committed or not)
  isn't appended. It is answered **after that latest record has committed**: 200 with
  `X-KV-Superseded: true`; if that commit times out, 504. So a client is never told a write
  won that may itself be lost.
- **Without a stamp from the client**, a write is newer than everything before it: "last
  arrival wins". LWW changes the outcome only for writes that carry an earlier stamp,
  retries above all. The design says so plainly.
- **Reads** keep choosing the newest `(epoch, LSN)` among R replicas (ADR-0021); within a
  shard that order and the stamp order are the same. Reads return `ETag: "<stamp>"`.
- **Conditional writes (the optional alternative strategy):** `If-Match: "<stamp>"` on PUT
  and DELETE, 412 if the key's latest stamp differs; `If-None-Match: *` on PUT, 412 if the
  key exists. LWW silently keeps one write; conditional writes let the client see the
  conflict and decide, which a read-modify-write needs. The MCP server uses both
  (ADR-0027).
- **M5:** migration compares stamps across shards, breaks ties by shard ID, and applies the
  HLC receive rule on the target leader, so its new writes stamp above migrated records.

**Why not vector clocks:** they detect writes that happened concurrently on different
replicas and keep both for someone to merge. With one leader per shard that never happens,
so they would cost space for no information. They belong with leaderless replication
(ADR-0020's runner-up).

**Why not client clocks (option 4):** a client whose clock is behind loses every race, and
one ahead pushes stamps forward. Option 3 gets the retry guarantee from a stamp the store
issued, with no client clock involved.

**Runner-up:** option 1. Reconsider if M5 migrates keys without comparing versions across
shards.

## Accepted downsides

- LWW drops the losing write silently (reported only by the header): fine for a key that is
  overwritten as a whole, wrong for a counter or a list; conditional writes are the answer
  there.
- A client that retries without sending the stamp back gets "last arrival wins", as in M3.
  go-ycsb and the smoke test don't retry, so this matters only for clients that do.
- 8 more bytes per record (36 → 44 of overhead).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A leader's clock runs far ahead | Low | Stamps from that leader are just high; the 5 s bound applies to client stamps only; EC2 clocks follow Amazon Time Sync |
| The leader compares with the applied map and misses an uncommitted write to the same key | Medium | Compare with the key's latest record in the log, kept per key at write time |

## Related

- ADR-0022 (format version), ADR-0021 (504 and retries), ADR-0020 (one leader per shard),
  ADR-0027 (conditional writes in the MCP tools).
