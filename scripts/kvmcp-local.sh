#!/bin/sh
# Starts the MCP server (cmd/kvmcp, ADR-0027) against the local cluster for an MCP client
# such as Claude Code. The key is read from secrets/ when the server starts, so it never
# sits in the client's configuration (`claude mcp add` stores --env values in ~/.claude.json).
#   KV_KEY_FILE  key file (default secrets/mcp.key: read, write; secrets/mcp-delete.key adds delete)
#   KV_URL       coordinator (default http://localhost:8080)
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
KEY_FILE=${KV_KEY_FILE:-$ROOT/secrets/mcp.key}
[ -s "$KEY_FILE" ] || { echo "no key in $KEY_FILE (run make keys)" >&2; exit 1; }
[ -x "$ROOT/kvmcp" ] || (cd "$ROOT/cmd/kvmcp" && go build -o ../../kvmcp .) >&2
KV_API_KEY=$(cat "$KEY_FILE") KV_URL=${KV_URL:-http://localhost:8080} exec "$ROOT/kvmcp"
