# ADR-0033: Rebalancing algorithm

- Status: **Accepted** — 2026-11-28 by Iryna Velmakina (author) in chat ("Accept and
  commit"), with the AI-review verdicts she confirmed ("Yes, apply them"). Drafted with
  Claude Code; revised after the AI design review (`docs/m5/ai-review.md`).
- Date: 2026-11-27
- Supersedes in part ADR-0013: "the coordinator adds an unknown ID to the ring" at
  registration. A new set now waits as *pending* until a migration adds it.
- Supersedes in part ADR-0022: record format 3 adds two record kinds (*freeze* and
  *purge*); format 2 records are still read.

## Context

The brief: when a shard joins, the coordinator adds it to the consistent hashing ring; a
background job moves only the keys that now belong to a different shard; the old shard
keeps serving reads until migration completes; show that distribution becomes more even.

What exists:
- **Ring:** xxHash64, 128 virtual nodes per shard, positions `xxhash64(shardID + "#" + i)`
  (ADR-0011). Adding a third shard moves ≈ 1/3 of the keys, taken evenly from both old
  shards; removing it moves its keys back. `docs/m2/ring-distribution.md` measured 3 and
  5 shards within 10% of the mean.
- **Registration adds a shard to the ring at once** (ADR-0013). Its accepted downside:
  "keys read as missing until M5's migration exists".
- **Per shard:** one leader orders writes; every record has `(epoch, LSN)`, an HLC stamp
  and a hash chain; followers pull the leader's log from a position (ADR-0022, ADR-0023).
  **The pull returns records the leader has appended but not yet committed**: a follower's
  next pull is its ack, and the leader keeps a separate commit point. A record that never
  committed can later be cut away by a new leader (ADR-0023 step 5). The log is never
  compacted. Deletes are tombstones with their stamp.
- **ADR-0028 promised for M5:** migration compares stamps across shards, breaks ties by
  shard ID, and applies the HLC receive rule on the target leader.
- **Data size:** ≈ 100,000 records × 1 KB in the benchmark; a third is ≈ 33 MB.

## Options

| | 1. Copy, then switch with a fenced cutover per source | 2. Switch writes first, read both shards until copied | 3. Dual writes to old and new | 4. Stop the world |
| --- | --- | --- | --- | --- |
| Who serves a moving key during the copy | the old shard, reads and writes | writes: the new shard; reads: both, newest stamp wins | both | nobody |
| Brief: old shard serves reads until done | yes, until that source's cutover | yes (alongside the new one) | yes | no |
| Write availability | writes to moving keys wait ≤ 2 s at cutover | none lost | none lost, but a write can half-succeed | minutes of 503 |
| Read cost during migration | unchanged | 2× for moving keys (4 replica reads instead of 2) | unchanged | — |
| Correctness hinges on | the source refusing writes to moving keys at a replicated freeze point, and the target reaching it | comparing HLC stamps from two leaders whose clocks differ by ms; the switch moment races in-flight writes | two commits agreeing | nothing |
| Reuses | the log-pull API (ADR-0023) for the tail | HLC comparison (ADR-0028) | — | — |
| How it is done elsewhere | Redis Cluster slot migration (MIGRATING/IMPORTING), Vitess, CockroachDB range moves | Dynamo-style hinted reads | rarely, for this reason | small systems |

**Where the cutover is fenced** (for option 1):

| | a. At the coordinator: hold new writes, read the source's last LSN, switch | **b. At the source leader: a replicated freeze record, after which it refuses writes to the moving keys** |
| --- | --- | --- |
| Writes already in flight to the source when the hold starts | can commit after the LSN was read and be lost or reordered (AI review W1) | **can't: they are either before the freeze record (and reach the target) or refused after it** |
| Survives a source leader change mid-cutover | no | **yes: the freeze record is in the log, so the new leader refuses too** |

**How the copy reads the source:**

| | a. Replay the source's whole log from LSN 0 | **b. Current state at the commit point, then the committed log tail** |
| --- | --- | --- |
| Volume | every version ever written (never compacted): several hundred MB per shard after a benchmark | one record per moving key (≈ 17 MB per source), plus the tail |
| Ordering | trivially correct | correct if the state is taken at a known commit point |

## Decision

**Option 1, fenced at the source (b), with copy method b.** Per migration (a join or a
leave):

1. **Pending.** A set that registers and isn't on the ring is *pending*: it gets a leader
   and heartbeats, but no traffic. An admin call (`admin` scope; the executor or a person)
   starts a migration naming the ring change.
2. **Plan.** The coordinator keeps two rings, `before` and `after`, and persists the
   migration (phase, rings, per-source progress, and for a scale action the request rate
   that triggered it and the action's end time, ADR-0032) in its log, as it does for
   leaders. A key is *moving* if its owner differs between them. One migration at a time.
3. **Copy.** For each source, the target's leader asks the source's leader for its
   **commit point `C₀`, taken once before the first page**, then for its current records of
   the keys moving to it as of `C₀`, in pages (values and tombstones, with stamps; a page
   reads only records at or below `C₀`). It applies them through its normal write path
   with the record's own stamp: applied only if newer than what it holds (ties broken by
   the source shard's ID), replicated with W = 2, its HLC advanced past each stamp (the
   receive rule). The copy is throttled (records per second, a setting).
4. **Tail.** It then pulls the source leader's log from `C₀` with a filter "keys moving to
   me". **The source serves the filtered tail only up to its commit point**, never
   appended-but-uncommitted records, so the target can't commit a write the source later
   cuts away (AI review W2). During 3 and 4, every read and write of a moving key still
   goes to the source.
5. **Cutover, per source.** When the tail's lag is small, the coordinator asks the source
   leader to **freeze** the moving keys. The leader appends a *freeze* record (the ring
   change's ID) at LSN `F` through the normal replicated path. From the moment it appends
   it, the leader answers **409 "moved"** to writes of those keys, and so does any later
   leader whose log has it. Records before `F` commit before `F` (commits go in LSN
   order), so once `F` has committed, every write the source accepted for those keys has
   too. The coordinator holds writes it gets for those keys meanwhile, waits until the
   target has applied the source's committed tail through `F`, switches those keys to
   `after` (persisted), and releases the held writes to the target. A write already sent
   to the source either committed before `F` (so it reaches the target) or got 409, and
   the coordinator retries it on the target after the switch. Held writes wait ≤ 2 s; if
   the target isn't there by then, the coordinator writes an *unfreeze* record on the
   source (same record kind, a flag), releases the held writes to it, and retries later.
6. **Reads at cutover.** Reads of that source's moving keys switch together with its
   writes, at its cutover, not at the end of the whole migration: from that moment the
   target holds every committed write of those keys and the source refuses new ones, so
   reading the source any longer would serve stale data. "The old shard serves reads until
   migration completes" holds per key range (AI review §3.3).
7. **Purge.** After every source has cut over, each source leader writes **purge** records
   for the keys that left, in batches of at most 1,000 keys per record (≈ 17 records for a
   source of ≈ 17,000 moving keys), replicated like any other; replicas drop those keys
   from memory without a tombstone. On replay, a purge record costs one map delete per key.
   Key counts and the dashboard's distribution change then.
8. **Done.** The ring is `after`; for a leave, the removed set is *retired* and can be
   stopped (ADR-0035).

**Record format 3** adds the *freeze* and *purge* kinds. Format 2 records are read as
before. An M4 binary meeting a format 3 record refuses the log at start, which is what the
format byte is for (ADR-0022), instead of misreading a new op.

**Removal** is the same with the leaving set as the only source and the remaining sets as
targets. **Only extra sets can leave** (shard-3 in M5): the base sets 1 and 2 are the
minimum capacity (ADR-0035), so the brief's "adding or removing shards" is shown by adding
and removing shard-3 (AI review §3.10).

**Why not option 2:** it needs no write hold, but it doubles reads of a third of the keys
for the whole migration, and its correctness rests on two leaders' clocks: a write to the
new shard can get a lower stamp than the last write to the old one when the new leader's
clock is a few ms behind, and the newer write then loses. Fixing that needs the same kind
of fence at the switch, so option 2 ends up paying for both.

**Restart and abort:** a coordinator restart resumes from the persisted phase (copy and
tail are idempotent: applying a record that isn't newer is a no-op). Before any cutover a
migration can be aborted (`abort_migration`) and the target's copied keys are purged; after
a cutover it can only finish.

**Runner-up:** option 2. Reconsider if a 2 s write hold on moving keys proves too long for
a client, or if the tail can't catch up under load.

## Accepted downsides

- Writes to moving keys wait up to 2 s at each cutover (two cutovers for a join, one per
  source). Reads and other keys don't wait.
- The tail lags the source by its commit wait: a record reaches the target only after a
  source follower has it.
- The source log keeps every copied, frozen and purged record (no compaction, ADR-0022);
  purge frees memory, not disk.
- Migration adds load on the source leaders (scan, tail) and target (writes with W = 2);
  throttled to a budget on the source's write p99, and scaling decisions pause meanwhile
  (ADR-0032).
- The coordinator now routes by two rings during a migration: more code in the request
  path.
- M4 binaries can't read an M5 log (format 3); M5 starts from empty volumes, as M3 and M4
  did.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A write in flight to the source commits after the switch and is lost | High | Fence at the source: writes to moving keys after the committed freeze record get 409; the target must reach `F` before the switch. A test injects a write between the hold and the freeze, and one during the freeze's commit wait |
| The target commits a record the source never committed | High | The filtered tail stops at the source's commit point; a test kills the source leader with an uncommitted write and checks it never appears on the target |
| A copied old value overwrites a newer write on the target | High | Apply only if the stamp is newer (ties by shard ID); the target's HLC moves past every stamp it receives |
| Source leader changes during a copy or cutover | Medium | The copy restarts from the new leader's commit point; the freeze record is replicated, so the new leader keeps refusing; idempotent applies |
| Purge reaches keys that didn't move | High | Purge lists are computed from the two persisted rings, and a test compares key sets before and after |
| Coordinator dies in a cutover with writes held | Medium | Held writes fail with 503 at its death like any request; on restart the persisted phase and the freeze records say which sources are switched |

## Related

- Implemented in: `internal/ring`, `internal/coordinator` (migration, routing by two rings,
  holds), `internal/shard` (state at a commit point, committed filtered pull, apply with
  stamp, freeze, purge), `internal/store` (record format 3).
- Uses: ADR-0011 (ring), ADR-0022/0023 (log, pull), ADR-0028 (stamps). Used by: ADR-0032,
  ADR-0035.
