# Spec Delta

## Purpose

A shard keeps its data across restarts without replicas.

## ADDED Requirements

### Requirement: Append-only log
Every put and delete SHALL be appended to a log on the instance's disk and flushed before
the shard answers.

#### Scenario: Write is durable
- **WHEN** a shard returns success for a put
- **THEN** the record is in the log file on disk

### Requirement: Failed writes leave the log clean
If appending a record fails, the shard SHALL cut the log back to the end of the last good
record before answering with an error, and SHALL stop accepting writes if it can't.

#### Scenario: Write error
- **WHEN** writing or flushing a put fails
- **THEN** the put returns an error, the log ends at the last good record, and later puts are appended right after it

### Requirement: Recovery on start
A shard SHALL rebuild its in-memory map by replaying the log at start. A damaged record at
the end of the log (a crash during an append) SHALL be dropped; damage followed by more
records SHALL stop the shard from starting (ADR-0018).

#### Scenario: Restart keeps data
- **WHEN** a shard that holds keys is restarted
- **THEN** every key put and not deleted before the restart reads back with the same value

#### Scenario: Torn last record
- **WHEN** the last record in the log is incomplete or fails its checksum
- **THEN** replay keeps every record before it, skips it, and the shard starts

#### Scenario: Damage in the middle
- **WHEN** a record fails its checksum and more records follow it
- **THEN** the shard does not start, and the error names the log file and the offset of the damaged record
