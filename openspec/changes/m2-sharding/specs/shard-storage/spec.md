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

### Requirement: Recovery on start
A shard SHALL rebuild its in-memory map by replaying the log at start, stopping at the
first damaged record.

#### Scenario: Restart keeps data
- **WHEN** a shard that holds keys is restarted
- **THEN** every key put and not deleted before the restart reads back with the same value

#### Scenario: Torn last record
- **WHEN** the last record in the log is incomplete or fails its checksum
- **THEN** replay keeps every record before it, skips it, and the shard starts
