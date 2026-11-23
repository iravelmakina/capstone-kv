# Spec Delta

## Purpose

The public contract of the store: register a table, then put, read, check and delete its
items by key, over REST and JSON.

## ADDED Requirements

### Requirement: Contract file
The API SHALL be described in `api/openapi.yaml` (OpenAPI 3.1), and the coordinator's
responses SHALL match it.

#### Scenario: Contract test
- **WHEN** the contract test runs in CI
- **THEN** every public endpoint's responses validate against `api/openapi.yaml`

### Requirement: Table registration
The API SHALL register a table with a name, a partition key name and an optional sort key
name.

#### Scenario: New table
- **WHEN** a client sends `POST /v1/tables` with a new name and a partition key
- **THEN** the response is 201 and the table can be used

#### Scenario: Duplicate table
- **WHEN** a client registers a table whose name already exists
- **THEN** the response is 409 and the existing definition is unchanged

### Requirement: Item operations
The API SHALL support CREATE (`PUT`), READ (`GET`), EXISTS (`HEAD`) and DELETE (`DELETE`)
on `/v1/tables/{table}/items/{pk}` and, for tables with a sort key, on
`/v1/tables/{table}/items/{pk}/{sk}`.

#### Scenario: Put then read
- **WHEN** a client puts a JSON object at a key and then reads that key
- **THEN** the read returns 200 and the same object

#### Scenario: Exists
- **WHEN** a client sends `HEAD` for a key that was put
- **THEN** the response is 200 with no body; for a key never put it is 404

#### Scenario: Delete
- **WHEN** a client deletes a key that exists
- **THEN** the response is 204, and a following read returns 404

#### Scenario: Unknown table
- **WHEN** a client uses a table that was never registered
- **THEN** the response is 404

#### Scenario: Wrong key shape
- **WHEN** a client omits the sort key on a table that has one, or adds one to a table that doesn't
- **THEN** the response is 400

### Requirement: Health endpoint
Every node SHALL answer `GET /health` with 200 while it can serve requests.

#### Scenario: Health
- **WHEN** `GET /health` is called on the coordinator or a shard that is running
- **THEN** the response is 200
