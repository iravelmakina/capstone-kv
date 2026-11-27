# Spec Delta

## Purpose

AI agents use the store through an MCP server whose tools, limits and authorisation are
fixed by the server and enforced by the store, not left to the agent.

## ADDED Requirements

### Requirement: Tools
The MCP server SHALL expose `list_tables`, `read_item`, `item_exists`, `create_item`,
`update_item` and `delete_item` over stdio, each with an input schema, and nothing else: no
table creation, no internal API. `create_item` SHALL only create (it fails if the key
exists); `update_item` SHALL require the stamp the agent read and fail if the item changed
since.

#### Scenario: Read through MCP
- **WHEN** an MCP client calls `read_item` for an existing key
- **THEN** it gets the stored value with its stamp

#### Scenario: Create doesn't overwrite
- **WHEN** an MCP client calls `create_item` for a key that exists
- **THEN** the tool reports a conflict and the stored value is unchanged

#### Scenario: Update needs the current version
- **WHEN** an MCP client calls `update_item` with a stamp that is no longer the item's latest
- **THEN** the tool reports a conflict and the stored value is unchanged

### Requirement: Outcomes named
Tool results SHALL name the store's outcome: created, updated, deleted, superseded,
conflict, not found, not permitted, invalid, unavailable, or outcome unknown, rather than a
generic failure.

#### Scenario: Unknown key
- **WHEN** an MCP client calls `read_item` for a missing key
- **THEN** it gets a "not found" result, not a server failure

### Requirement: Authorisation
The server SHALL call the store with one API key from its environment. The default
configuration's key SHALL have `read` and `write` only. `delete_item` SHALL be annotated as
destructive (an advisory hint for the client) and SHALL succeed only with a key that has the
`delete` scope.

#### Scenario: Delete without the scope
- **WHEN** the server runs with the default key and a client calls `delete_item`
- **THEN** the tool returns "not permitted" and the item is still readable

### Requirement: Stored data is data
`read_item` SHALL return a value as a JSON field of a structured result marked as stored
data, truncated at 16 KB with a flag, and the server SHALL never follow, evaluate or pass on
instructions found in values.

#### Scenario: Large value
- **WHEN** a value of 40 KB is read through MCP
- **THEN** the result holds its first 16 KB and says it was truncated

### Requirement: Verified with a real client
The server SHALL be shown working with an MCP client (Claude Code) against the local
cluster: reading, creating, updating, and deleting with the delete-scoped key after the
client asked for confirmation.

#### Scenario: Demo recorded
- **WHEN** the demo is run
- **THEN** its transcript is saved in `docs/m4/evidence/mcp-demo.md`
