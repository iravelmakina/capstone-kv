# Spec Delta

## Purpose

Every record carries a stamp from its leader that orders writes to a key; a retried write
loses to any newer write (last-write-wins); a client can also make a write conditional on
the version it read, or on the key not existing.

## ADDED Requirements

### Requirement: Stamp per record
Every record SHALL carry a hybrid logical clock stamp (physical milliseconds and a logical
counter) in record format 2. The leader SHALL stamp each write with a value above every
stamp in its log, including after it was promoted. Format 1 records SHALL still be read,
with stamp 0.

#### Scenario: Stamps rise across a promotion
- **WHEN** a leader is replaced and the new leader accepts a write
- **THEN** that write's stamp is higher than every stamp written under the previous epoch

### Requirement: Stamp returned on every write
Every answer to a write that reached the leader SHALL carry the write's stamp in
`X-KV-Timestamp`, including 504.

#### Scenario: Stamp on a timeout
- **WHEN** a put times out with 504
- **THEN** the 504 carries the stamp the leader gave the write

### Requirement: Client stamp
A write MAY carry `X-KV-Timestamp` (a stamp the store returned earlier, typically for a
retry). The leader SHALL refuse it with 400 when its physical part is more than 5 seconds
ahead of the leader's physical clock, and otherwise use it as the write's stamp and move its
own clock forward to it.

#### Scenario: Stamp in the future
- **WHEN** a put carries a stamp 60 seconds ahead
- **THEN** it returns 400 and nothing is written

### Requirement: Last-write-wins
For each key, the record with the higher stamp SHALL win. The leader SHALL not append a
write whose stamp is not higher than the stamp of the key's latest record; it SHALL answer
such a write only after that latest record has committed, with 200 and
`X-KV-Superseded: true`, or with 504 if that commit times out.

#### Scenario: Late retry loses
- **WHEN** client A's put times out with 504 and stamp t1, client B then writes the same key, and A retries with t1
- **THEN** A's retry returns 200 with `X-KV-Superseded: true` and every read returns B's value

#### Scenario: Superseded only by a committed write
- **WHEN** a write with an older stamp arrives while the newer write to its key is still waiting for its commit, and that commit times out
- **THEN** the older write is answered 504, not superseded

#### Scenario: Write without a stamp
- **WHEN** a put carries no `X-KV-Timestamp`
- **THEN** it gets a stamp above every earlier one and is written

### Requirement: Version on reads
Reads SHALL return `ETag: "<stamp>"` and `X-KV-Timestamp` for the version they return.

#### Scenario: Version returned
- **WHEN** a client reads a key
- **THEN** the response carries the ETag and stamp of the value it returns

### Requirement: Conditional writes
A PUT or DELETE MAY carry `If-Match: "<stamp>"`; the leader SHALL apply it only if that is
still the key's latest stamp, and otherwise answer 412. A PUT MAY carry `If-None-Match: *`;
the leader SHALL apply it only if the key doesn't exist (or its latest record is a
tombstone), and otherwise answer 412.

#### Scenario: Lost update prevented
- **WHEN** two clients read the same stamp and both put with `If-Match` on it
- **THEN** one put succeeds and the other returns 412

#### Scenario: Create only
- **WHEN** a put with `If-None-Match: *` targets an existing key
- **THEN** it returns 412 and the value is unchanged
