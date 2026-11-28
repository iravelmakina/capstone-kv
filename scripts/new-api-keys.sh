#!/usr/bin/env bash
# Makes a fresh set of API keys (ADR-0027) in secrets/, which git ignores:
#   secrets/api-keys.json   hashes, names and scopes: what the coordinator loads
#   secrets/<name>.key      each key's value, for the clients that use it
#
#   ops         read, write, delete, admin   smoke tests, failure tests, benchmark
#   probe       read                         the monitor's probe (ADR-0029)
#   mcp         read, write                  the MCP server's default key
#   mcp-delete  read, write, delete          the MCP demo of a delete
#   ops-agent   status                       the ops agent: cluster state only (M5, ADR-0034)
#
# Values are random (32 bytes, hex) and never printed. In AWS the secrets are made from
# these files (`make secrets`, in a lab session): velmakina-kv/api-keys (the hashes),
# velmakina-kv/ops-key and velmakina-kv/probe-key (one value each). Refuses to overwrite
# existing keys unless FORCE=1, so a running cluster's keys aren't replaced by accident.
set -euo pipefail
DIR=${1:-secrets}
if [ -e "$DIR/api-keys.json" ] && [ "${FORCE:-0}" != 1 ]; then
  echo "$DIR/api-keys.json exists; FORCE=1 to make new keys"
  exit 0
fi
mkdir -p "$DIR"
chmod 700 "$DIR"
umask 077

entries=()
for spec in "ops:read,write,delete,admin" "probe:read" "mcp:read,write" "mcp-delete:read,write,delete" "ops-agent:status"; do
  name=${spec%%:*}
  scopes=${spec#*:}
  value=$(openssl rand -hex 32)
  printf '%s' "$value" > "$DIR/$name.key"
  hash=$(printf '%s' "$value" | openssl dgst -sha256 -r | cut -d' ' -f1)
  entries+=("{\"name\":\"$name\",\"sha256\":\"$hash\",\"scopes\":[\"${scopes//,/\",\"}\"]}")
done
( IFS=,; printf '[%s]\n' "${entries[*]}" ) > "$DIR/api-keys.json"
chmod 644 "$DIR/api-keys.json"   # hashes only: readable by the coordinator's container
echo "made keys ops, probe, mcp, mcp-delete, ops-agent in $DIR/ (values not shown)"
