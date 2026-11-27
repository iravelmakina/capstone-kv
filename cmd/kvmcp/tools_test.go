package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeStore is a coordinator stand-in with the rules the tools rely on: keys and scopes,
// stamps as ETags, If-Match, If-None-Match, and a superseding header.
type fakeStore struct {
	mu    sync.Mutex
	items map[string]string // path -> JSON
	stamp map[string]uint64
	next  uint64
}

var fakeKeys = map[string][]string{"rw": {"read", "write"}, "rwd": {"read", "write", "delete"}}

func (f *fakeStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scopes, ok := fakeKeys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		http.Error(w, `{"error":"a valid API key is required"}`, 401)
		return
	}
	need := map[string]string{"GET": "read", "HEAD": "read", "PUT": "write", "DELETE": "delete"}[r.Method]
	if !contains(scopes, need) {
		http.Error(w, `{"error":"this key may not `+need+`"}`, 403)
		return
	}
	if r.URL.Path == "/v1/tables" {
		w.Write([]byte(`[{"name":"users","partitionKey":"userId"}]`))
		return
	}
	p := r.URL.Path
	val, exists := f.items[p]
	tag := `"` + strconv.FormatUint(f.stamp[p], 10) + `"`
	switch r.Method {
	case "GET", "HEAD":
		if !exists {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", tag)
		w.WriteHeader(200)
		if r.Method == "GET" {
			w.Write([]byte(val))
		}
	case "PUT":
		if r.Header.Get("If-None-Match") == "*" && exists {
			http.Error(w, `{"error":"the item exists"}`, 412)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && (!exists || m != tag) {
			http.Error(w, `{"error":"the item changed"}`, 412)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.next++
		f.items[p], f.stamp[p] = string(b), f.next
		w.Header().Set("ETag", `"`+strconv.FormatUint(f.next, 10)+`"`)
		w.WriteHeader(200)
	case "DELETE":
		if !exists {
			w.WriteHeader(404)
			return
		}
		delete(f.items, p)
		w.WriteHeader(204)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// session starts the MCP server against a fake store with the given key and connects an
// in-memory client to it.
func session(t *testing.T, key string) (*mcp.ClientSession, *fakeStore) {
	t.Helper()
	f := &fakeStore{items: map[string]string{}, stamp: map[string]uint64{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	server := newServer(&storeClient{base: srv.URL, key: key, http: srv.Client()})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, f
}

// callTool calls a tool and decodes its structured result.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (Result, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%s: structured result %s: %v", name, raw, err)
	}
	return r, res.IsError
}

func key(pk string) map[string]any { return map[string]any{"table": "users", "partitionKey": pk} }

func with(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

// The tool list: exactly the six tools, with the hints ADR-0027 names.
func TestToolsAndHints(t *testing.T) {
	cs, _ := session(t, "rw")
	got := map[string]*mcp.ToolAnnotations{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		got[tool.Name] = tool.Annotations
	}
	want := []string{"list_tables", "read_item", "item_exists", "create_item", "update_item", "delete_item"}
	if len(got) != len(want) {
		t.Fatalf("tools %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if got[name] == nil {
			t.Fatalf("missing tool %s", name)
		}
	}
	if !got["read_item"].ReadOnlyHint || !got["item_exists"].ReadOnlyHint || !got["list_tables"].ReadOnlyHint {
		t.Error("read tools must be read-only")
	}
	if d := got["delete_item"].DestructiveHint; d == nil || !*d {
		t.Error("delete_item must be destructive")
	}
	if d := got["create_item"].DestructiveHint; d == nil || *d {
		t.Error("create_item must not be destructive")
	}
}

// list_tables returns typed tables (its output passes the SDK's schema validation).
func TestListTables(t *testing.T) {
	cs, _ := session(t, "rw")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_tables", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var tr TablesResult
	json.Unmarshal(raw, &tr)
	if tr.Outcome != "found" || len(tr.Tables) != 1 || tr.Tables[0].Name != "users" || tr.Tables[0].PartitionKey != "userId" {
		t.Fatalf("list_tables = %s", raw)
	}
}

// Specs "Read through MCP", "Create doesn't overwrite", "Update needs the current version".
func TestCreateReadUpdate(t *testing.T) {
	cs, f := session(t, "rw")
	if r, _ := callTool(t, cs, "create_item", with(key("u1"), "value", map[string]any{"name": "Ira"})); r.Outcome != "created" {
		t.Fatalf("create: %+v", r)
	}
	if r, isErr := callTool(t, cs, "create_item", with(key("u1"), "value", map[string]any{"name": "X"})); r.Outcome != "conflict" || isErr {
		t.Fatalf("create over an existing item: %+v (error %v), want conflict", r, isErr)
	}
	r, _ := callTool(t, cs, "read_item", key("u1"))
	if r.Outcome != "found" || r.Value != `{"name":"Ira"}` || !r.StoredData || r.Version == "" {
		t.Fatalf("read: %+v", r)
	}
	v1 := r.Version
	if r, _ := callTool(t, cs, "update_item", with(with(key("u1"), "value", map[string]any{"name": "Iryna"}), "version", v1)); r.Outcome != "updated" {
		t.Fatalf("update with the read version: %+v", r)
	}
	if r, _ := callTool(t, cs, "update_item", with(with(key("u1"), "value", map[string]any{"name": "Old"}), "version", v1)); r.Outcome != "conflict" {
		t.Fatalf("update with a stale version: %+v, want conflict", r)
	}
	if f.items["/v1/tables/users/items/u1"] != `{"name":"Iryna"}` {
		t.Fatalf("stored %s", f.items["/v1/tables/users/items/u1"])
	}
	// version is a required argument: the SDK refuses the call before the store is asked.
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "update_item",
		Arguments: with(key("u1"), "value", map[string]any{"a": 1})}); err == nil {
		t.Fatal("update without a version was accepted")
	}
}

// Specs "Unknown key" and "Delete without the scope".
func TestNotFoundAndNotPermitted(t *testing.T) {
	cs, f := session(t, "rw")
	if r, isErr := callTool(t, cs, "read_item", key("missing")); r.Outcome != "not_found" || isErr {
		t.Fatalf("missing item: %+v (error %v), want not_found, not an error", r, isErr)
	}
	if r, _ := callTool(t, cs, "item_exists", key("missing")); r.Outcome != "not_found" {
		t.Fatalf("exists on a missing item: %+v", r)
	}
	callTool(t, cs, "create_item", with(key("u1"), "value", map[string]any{"a": 1}))
	r, isErr := callTool(t, cs, "delete_item", key("u1"))
	if r.Outcome != "not_permitted" || !isErr {
		t.Fatalf("delete with the default key: %+v (error %v), want not_permitted", r, isErr)
	}
	if _, ok := f.items["/v1/tables/users/items/u1"]; !ok {
		t.Fatal("item gone after a refused delete")
	}
}

func TestDeleteWithTheDeleteKey(t *testing.T) {
	cs, f := session(t, "rwd")
	callTool(t, cs, "create_item", with(key("u1"), "value", map[string]any{"a": 1}))
	if r, _ := callTool(t, cs, "delete_item", key("u1")); r.Outcome != "deleted" {
		t.Fatalf("delete: %+v", r)
	}
	if len(f.items) != 0 {
		t.Fatal("item still stored")
	}
}

// Spec "Large value": the first 16 KB, flagged.
func TestLargeValueTruncated(t *testing.T) {
	cs, f := session(t, "rw")
	big := `{"blob":"` + strings.Repeat("x", 40<<10) + `"}`
	f.items["/v1/tables/users/items/big"], f.stamp["/v1/tables/users/items/big"] = big, 1
	r, _ := callTool(t, cs, "read_item", key("big"))
	if !r.Truncated || len(r.Value) != maxValue {
		t.Fatalf("truncated=%v len=%d, want true and %d", r.Truncated, len(r.Value), maxValue)
	}
}

func TestWrongKey(t *testing.T) {
	cs, _ := session(t, "nope")
	if r, isErr := callTool(t, cs, "read_item", key("u1")); r.Outcome != "not_permitted" || !isErr {
		t.Fatalf("unknown key: %+v", r)
	}
}

func TestOutcomeNames(t *testing.T) {
	superseded := http.Header{}
	superseded.Set("X-KV-Superseded", "true")
	for status, want := range map[int]string{200: "success", 204: "success", 404: "not_found", 401: "not_permitted",
		403: "not_permitted", 412: "conflict", 400: "invalid", 503: "unavailable", 504: "outcome_unknown", 500: "error"} {
		if got := outcome(status, http.Header{}); got != want {
			t.Errorf("outcome(%d) = %s, want %s", status, got, want)
		}
	}
	if got := outcome(200, superseded); got != "superseded" {
		t.Errorf("superseded write = %s", got)
	}
}
