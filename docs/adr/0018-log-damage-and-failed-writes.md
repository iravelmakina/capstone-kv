# ADR-0018: Damaged logs and failed writes

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the AI design review verdicts (`docs/m2/ai-review.md`, §1.2, §2.3) agreed in chat.
- Date: 2026-11-24
- Supersedes: in ADR-0013, the replay rule "replay stops at the first bad one" (Risks table). The rest of ADR-0013 stands.
- Deferred items settled by ADR-0022: header checksum and replica re-sync done in M3; group commit moved to M5.

## Context

Two ways the shard's log (ADR-0013) can lose acknowledged data today:

1. **Damage in the middle is treated like a torn tail.** Replay stops at the first bad
   record and the file is truncated there. If a record in the middle is damaged (a bad
   sector, a bit flip), every record after it, all of them acknowledged to clients, is
   silently deleted, and the shard starts as if nothing happened.
2. **A failed write leaves garbage in the log.** If `write` or `fsync` fails part-way, the
   bytes already written stay at the end of the file, and the next successful put is
   appended after them. At the next start, replay stops at the garbage and drops that later,
   acknowledged put.

A crash during an append can only damage the **last** record. Anything else is a different
failure and needs a person.

## Options

| | 1. Keep as is | 2. Tail vs middle; truncate on failed write | 3. Embedded engine (Pebble, bbolt) |
| --- | --- | --- | --- |
| Silent loss of acknowledged writes | yes | no | no |
| Code to explain | — | ~40 lines | a library's internals |
| Groundwork for M3's replication log | — | same log | none |

## Decision

**Option 2.**

- **Torn tail (a crash during an append):** the damaged record is the last thing in the
  file: its header is cut short, its length runs to or past the end of the file, or its
  checksum fails and nothing follows it. Replay keeps everything before it, truncates it,
  and the shard starts, as before.
- **Damage in the middle:** a record fails its checksum (or decoding) and more bytes follow.
  The shard **refuses to start**, naming the file and offset. A person decides: restore,
  or truncate by hand and accept the loss. Never silently.
- **Failed write:** if `write` or `fsync` fails, the store cuts the file back to the end of
  the last good record before answering with an error. If even that fails, the store stops
  accepting writes (every put and delete returns an error) until restarted, so nothing is
  appended after garbage.

**Runner-up:** option 3 (the AI reviewer's suggestion). Refuted: the course grades building
the store, M3's replication log is built on this one, and a library's internals can't be
explained "without notes". Its valid concerns are covered above; record versions/LSNs and
group commit come with M3, where replication needs them.

## Accepted downsides

- A shard with damage in the middle of its log stays down (its keys answer 503) until a
  person acts. Without replicas that is the honest answer; M3's replicas will let it
  re-sync instead.
- A damaged **length field** that points past the end of the file looks like a torn tail,
  and the records after it are dropped. The checksum doesn't cover the header; M3's record
  format can add a header checksum.

## Related

- Implemented in: `internal/store/`.
- Source: AI design review §1.2 and §2.3 (`docs/m2/ai-review.md`).
