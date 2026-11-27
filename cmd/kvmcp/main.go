// Command kvmcp is the MCP server of capstone-kv (ADR-0027): it lets an AI agent read,
// check, create, update and delete items through the store's public API, over stdio.
//
//	KV_URL=http://localhost:8080 KV_API_KEY=$(cat secrets/mcp.key) kvmcp
//
// The limits are the store's, not this program's: every call carries the key from
// KV_API_KEY, and the coordinator refuses what the key's scopes don't allow. The default
// key (mcp) may read and write but not delete. Writes are never blind: create_item fails
// if the item exists, and update_item needs the version the agent read. Stored values are
// returned as data in a structured result, at most 16 KB, and never interpreted.
//
// It is its own Go module, so the SDK and its dependencies stay out of the store's binary.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	key := os.Getenv("KV_API_KEY")
	if key == "" {
		log.Fatal("KV_API_KEY is required: the store refuses requests without a key")
	}
	base := strings.TrimRight(envOr("KV_URL", "http://localhost:8080"), "/")
	store := &storeClient{base: base, key: key, http: &http.Client{Timeout: 10 * time.Second}}
	server := newServer(store)
	// stdio: the MCP client starts this program and talks over its stdin and stdout, so
	// logs must go to stderr only.
	log.SetOutput(os.Stderr)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
