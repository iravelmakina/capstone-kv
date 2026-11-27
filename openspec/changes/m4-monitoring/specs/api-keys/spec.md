# Spec Delta

## Purpose

Only callers holding an API key with the right scope can read, write, delete or create
tables through the public API, and every request is attributed to a key's name.

## ADDED Requirements

### Requirement: Keys with scopes
The coordinator SHALL require `Authorization: Bearer <key>` on every public route and
SHALL check the key's scopes: `read` for GET and HEAD, `write` for PUT on items, `delete`
for DELETE on items, `admin` for creating tables. It SHALL answer 401 without a valid key
and 403 without the scope. `/health`, the metrics port and the internal API SHALL not
require a key.

#### Scenario: No key
- **WHEN** a client reads an item without an Authorization header
- **THEN** the coordinator returns 401

#### Scenario: Missing scope
- **WHEN** a client whose key has only `read` and `write` deletes an item
- **THEN** the coordinator returns 403 and the item is still readable

### Requirement: Keys stored as hashes
The coordinator SHALL load keys as SHA-256 hashes with their names and scopes, from a file
(written from a Secrets Manager secret at boot in AWS), and SHALL reload the file when it
changes. Key values SHALL never be in the repository, in a template, in a workflow file or
in a log line. The smoke test, the benchmark client and the probe SHALL read their key at
run time from a second secret.

#### Scenario: Key rotated without a restart
- **WHEN** a key is removed from the keys file while the coordinator runs
- **THEN** within 15 seconds requests with that key return 401, and other keys keep working

### Requirement: Requests attributed
Every request SHALL be logged and counted with the name of the key that made it
(`principal`).

#### Scenario: Who deleted it
- **WHEN** an item is deleted
- **THEN** the coordinator's log line for that request names the key that deleted it
