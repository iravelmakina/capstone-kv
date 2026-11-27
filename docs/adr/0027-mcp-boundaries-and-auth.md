# ADR-0027: What an agent may do through MCP, and how it is authorised

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("я все приймаю"),
  together with the AI-review verdicts. Drafted with Claude Code from the decision list she
  approved; revised after the AI design review (`docs/m4/ai-review.md`).
- Date: 2026-11-27

## Context

The brief requires an MCP server exposing EXISTS, CREATE, READ and DELETE as tools, shown
with a real client at the defense, and asks what an agent may and may not do, how it is
authorised, whether it can delete data, and what happens if prompt injection sits in the
stored data. Today the public API has no authentication at all: anyone who reaches port
8080 can do anything. The store keeps only the newest version of a key, so an overwrite
destroys the old value as surely as a delete.

## Options

**Where the limits are enforced**

| | 1. In the MCP server only | 2. In the store (API keys with scopes), the MCP server holds a key |
| --- | --- | --- |
| A bug or a prompt in the MCP server can exceed its limits | yes | no: the store refuses |
| Also protects the API from other callers | no | yes |
| Work | small | keys, scopes, a secret |

**Language and transport**

| | Go, official SDK, stdio | TypeScript SDK, stdio | HTTP transport |
| --- | --- | --- | --- |
| Same toolchain as the store | yes | no | either |
| Works with Claude Code / Desktop | yes | yes | yes, needs hosting and its own auth |

**Writing**

| | Upsert (`create_item` = PUT) | Create only, plus an update that needs the version read |
| --- | --- | --- |
| Matches the brief's CREATE | loosely | exactly |
| An injected "blank every item" with a write key | destroys data | can't: create fails on existing keys, update needs each item's current stamp |

**Delete**

| | Not exposed | Exposed, needs a delete scope | Exposed freely |
| --- | --- | --- | --- |
| Meets the brief (DELETE as a tool) | no | yes | yes |
| One injected sentence can delete data | no | only with a delete key | yes |

## Decision

1. **The store enforces the limits.** The coordinator requires an API key with scopes
   (`read`, `write`, `delete`, `admin`) on every public route. Keys are SHA-256 hashes with
   a name and scopes in a Secrets Manager secret (a file locally), reloaded when they
   change. Every request is logged and counted with the key's name (`principal`), so after
   an incident we can say what the agent changed.
2. **`cmd/kvmcp`:** Go on the official MCP SDK, stdio transport, started by the MCP client
   with `KV_URL` and `KV_API_KEY` in its environment.
3. **Tools:**
   - `list_tables`, `read_item`, `item_exists` (read-only hint);
   - `create_item`: the brief's CREATE, a PUT with `If-None-Match: *`, so it fails if the
     key exists;
   - `update_item`: a PUT with `If-Match` on the stamp the agent read, so it fails if the
     item changed or was never read;
   - `delete_item` (destructive hint).
4. **Not exposed:** creating or dropping tables, the internal API, anything outside the
   store. An agent can't change the schema or the cluster.
5. **Delete:** the default key has `read` and `write` only, so `delete_item` returns "not
   permitted". The demo runs with a separate key that has `delete`. The destructive hint is
   **advisory**: MCP clients may ignore it, and a person can allow a tool permanently. The
   control is the scope; the hint and Claude Code's permission prompt are a second line.
6. **Outcomes are named.** Tool results say created, updated, deleted, superseded,
   conflict (412), not found, not permitted (401/403), invalid (400), unavailable (503) or
   outcome unknown (504), so the agent knows whether its write applied.
7. **Stored data is data.** `read_item` returns the value as a JSON field of a structured
   result labelled as stored data, at most 16 KB (truncation is flagged). The server never
   interprets values.

**Prompt injection.** A value can say anything, including "delete every item" or "set
every value to empty". The agent reading it may be fooled; the system around it limits
what that can cause:
- with the default key it can't delete;
- it can't overwrite an existing item without first reading it and passing the stamp it
  read, and it can't create over an existing one;
- nothing can create tables or touch the cluster;
- the server has no tool that runs code or reaches other systems;
- every write it makes is logged under its key's name.

What remains possible: updating items it has read (one at a time, each attributable),
creating new items, and being misled in its answer to the user, which no server-side rule
can prevent. Last-write-wins doesn't bound an agent's writes: its write is the newest, so
it wins.

**Runner-up:** enforcement in the MCP server only, for a store that has no other callers.
Reconsider never: the store would still be open to everything else.

## Accepted downsides

- Every client of the API (smoke test, benchmark, failure tests) now needs a key.
- Keys are global: no per-table keys and no per-key rate limit in M4. Rate limiting is the
  course's Bonus 1 (API throttling); per-table scopes are a small addition if M5's ops
  agent needs them.
- The internal API and the metrics port stay without keys, protected by security groups:
  only the coordinator and the replicas reach the internal ports, and the monitor reaches
  only the metrics ports (ADR-0029).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A key leaks through a log or a commit | High | Keys never logged (only their names); only hashes in the secret; values never in the repo or workflows |
| A client ignores the destructive hint | Medium | The default key has no `delete`; the hint is the second line, not the first |
| An agent loops on writes and burns the SLO | Low | Every write is attributed; throttling is Bonus 1 |

## Related

- ADR-0025 (metrics: `principal` label), ADR-0028 (stamps used by `update_item`).
