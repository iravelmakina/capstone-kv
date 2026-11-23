package coordinator

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestContract checks the coordinator against api/openapi.yaml (spec "Contract test"):
//   - every operation in the file is exercised by at least one request below,
//   - every response status the coordinator returns is documented for that operation,
//   - error responses are JSON {"error": "..."}, as the Error schema says.
//
// Adding an operation to openapi.yaml without a request here fails the test, so the file
// and the code can't drift apart silently.
func TestContract(t *testing.T) {
	documented := loadOperations(t, "../../api/openapi.yaml")

	ready := newCluster(t, 2, true)
	notReady := newCluster(t, 2, false)
	for _, c := range []*cluster{ready, notReady} {
		call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
		call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"orders","partitionKey":"customerId","sortKey":"orderId"}`)
	}
	r, n := ready.coord.URL, notReady.coord.URL

	type step struct{ op, method, url, body string }
	steps := []step{
		{"health", "GET", r + "/health", ""},
		{"createTable", "POST", r + "/v1/tables", `{"name":"logs","partitionKey":"day"}`},
		{"createTable", "POST", r + "/v1/tables", `{"name":"logs","partitionKey":"day"}`}, // 409
		{"createTable", "POST", r + "/v1/tables", `{"name":""}`},                          // 400
		{"listTables", "GET", r + "/v1/tables", ""},
		{"getTable", "GET", r + "/v1/tables/users", ""},
		{"getTable", "GET", r + "/v1/tables/nope", ""}, // 404

		// Partition-key-only table.
		{"putItem", "PUT", r + "/v1/tables/users/items/u1", `{"name":"Ira"}`},
		{"putItem", "PUT", r + "/v1/tables/users/items/u1", `not json`},  // 400
		{"putItem", "PUT", r + "/v1/tables/nope/items/u1", `{"a":1}`},    // 404
		{"putItem", "PUT", n + "/v1/tables/users/items/u1", `{"a":1}`},   // 503
		{"getItem", "GET", r + "/v1/tables/users/items/u1", ""},          // 200
		{"getItem", "GET", r + "/v1/tables/users/items/missing", ""},     // 404
		{"getItem", "GET", r + "/v1/tables/orders/items/c1", ""},         // 400: needs sort key
		{"getItem", "GET", n + "/v1/tables/users/items/u1", ""},          // 503
		{"itemExists", "HEAD", r + "/v1/tables/users/items/u1", ""},      // 200
		{"itemExists", "HEAD", r + "/v1/tables/users/items/missing", ""}, // 404
		{"itemExists", "HEAD", r + "/v1/tables/orders/items/c1", ""},     // 400
		{"itemExists", "HEAD", n + "/v1/tables/users/items/u1", ""},      // 503
		{"deleteItem", "DELETE", r + "/v1/tables/users/items/u1", ""},    // 204
		{"deleteItem", "DELETE", r + "/v1/tables/users/items/u1", ""},    // 404
		{"deleteItem", "DELETE", r + "/v1/tables/orders/items/c1", ""},   // 400
		{"deleteItem", "DELETE", n + "/v1/tables/users/items/u1", ""},    // 503

		// Table with a sort key.
		{"putItemWithSortKey", "PUT", r + "/v1/tables/orders/items/c1/o1", `{"total":10}`},
		{"putItemWithSortKey", "PUT", r + "/v1/tables/users/items/u1/x", `{"a":1}`}, // 400: no sort key
		{"putItemWithSortKey", "PUT", r + "/v1/tables/nope/items/a/b", `{"a":1}`},   // 404
		{"putItemWithSortKey", "PUT", n + "/v1/tables/orders/items/c1/o1", `{"a":1}`},
		{"getItemWithSortKey", "GET", r + "/v1/tables/orders/items/c1/o1", ""},
		{"getItemWithSortKey", "GET", r + "/v1/tables/orders/items/c1/o2", ""},
		{"getItemWithSortKey", "GET", r + "/v1/tables/users/items/u1/x", ""},
		{"getItemWithSortKey", "GET", n + "/v1/tables/orders/items/c1/o1", ""},
		{"itemExistsWithSortKey", "HEAD", r + "/v1/tables/orders/items/c1/o1", ""},
		{"itemExistsWithSortKey", "HEAD", r + "/v1/tables/orders/items/c1/o2", ""},
		{"itemExistsWithSortKey", "HEAD", r + "/v1/tables/users/items/u1/x", ""},
		{"itemExistsWithSortKey", "HEAD", n + "/v1/tables/orders/items/c1/o1", ""},
		{"deleteItemWithSortKey", "DELETE", r + "/v1/tables/orders/items/c1/o1", ""},
		{"deleteItemWithSortKey", "DELETE", r + "/v1/tables/orders/items/c1/o1", ""},
		{"deleteItemWithSortKey", "DELETE", r + "/v1/tables/users/items/u1/x", ""},
		{"deleteItemWithSortKey", "DELETE", n + "/v1/tables/orders/items/c1/o1", ""},
	}

	exercised := map[string]bool{}
	for _, s := range steps {
		codes, ok := documented[s.op]
		if !ok {
			t.Fatalf("step uses %q, which isn't in openapi.yaml", s.op)
		}
		exercised[s.op] = true
		code, body := call(t, s.method, s.url, s.body)
		if !codes[code] {
			t.Errorf("%s %s -> %d, not documented for %s (documented: %v)", s.method, s.url, code, s.op, keys(codes))
		}
		if code >= 400 && s.method != "HEAD" {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(body), &e) != nil || e.Error == "" {
				t.Errorf("%s %s -> %d with body %q, want {\"error\": ...}", s.method, s.url, code, body)
			}
		}
	}
	for op := range documented {
		if !exercised[op] {
			t.Errorf("operation %s is in openapi.yaml but not exercised by this test", op)
		}
	}
}

// loadOperations returns operationId -> set of documented status codes, following
// path-level $refs into components/pathItems.
func loadOperations(t *testing.T, path string) map[string]map[int]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			PathItems map[string]map[string]any `yaml:"pathItems"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]map[int]bool{}
	for p, item := range doc.Paths {
		if ref, ok := item["$ref"].(string); ok {
			item = doc.Components.PathItems[strings.TrimPrefix(ref, "#/components/pathItems/")]
			if item == nil {
				t.Fatalf("%s: unresolved %s", p, ref)
			}
		}
		for _, method := range []string{"get", "put", "post", "delete", "head"} {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			codes := map[int]bool{}
			for status := range op["responses"].(map[string]any) {
				var c int
				json.Unmarshal([]byte(status), &c)
				codes[c] = true
			}
			ops[id] = codes
		}
	}
	return ops
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
