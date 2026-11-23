# ADR-0010: API protocol and format

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

The brief asks for an API to register a table and do CRUD over its records (EXISTS,
CREATE, READ, DELETE), optionally with a compound key. Three clients will use it: the
benchmark (M2), the MCP server (M4) and people testing by hand. Coordinator↔shard calls
are internal.

## Options

| | REST + JSON, OpenAPI 3 | gRPC + protobuf | GraphQL |
| --- | --- | --- | --- |
| Test by hand | `curl` | needs grpcurl and the .proto | a client or playground |
| Benchmark client | plain HTTP | generated stubs | awkward for point ops |
| Wire efficiency | text, larger | binary, smaller, HTTP/2 | text |
| Maps to MCP tools (M4) | one endpoint per tool | possible | possible |
| Contract file | OpenAPI YAML | .proto | schema |

## Decision

**REST with JSON bodies, described in `api/openapi.yaml` (OpenAPI 3.1), versioned under `/v1`.**

| Operation | Request | Success | Not found |
| --- | --- | --- | --- |
| Register table | `POST /v1/tables` `{name, partitionKey, sortKey?}` | 201 | 409 if it exists |
| CREATE (put) | `PUT /v1/tables/{t}/items/{pk}[/{sk}]`, body: JSON object | 200 | 404 if the table doesn't exist |
| READ | `GET` same path | 200 + value | 404 |
| EXISTS | `HEAD` same path | 200, no body | 404 |
| DELETE | `DELETE` same path | 204 | 404 |

`PUT` because a put is idempotent: repeating it leaves the same state. The sort key
segment is required exactly when the table defines a sort key. Internal endpoints
(`/internal/v1/…`: shard registration, shard key-value calls) use the same style and
aren't part of the public contract. `GET /health` on every node.

**Runner-up:** gRPC. Reconsider for coordinator↔shard traffic if HTTP overhead shows in
the benchmark.

## Accepted downsides

- JSON is larger and slower to parse than protobuf.
- No streaming; not needed for point operations.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Code drifts from `openapi.yaml` | Medium | A contract test in CI checks responses against the file |

## Related

- Implemented in: `api/openapi.yaml`, `internal/api/`.
- Uses: ADR-0012 (what the key segments mean).
