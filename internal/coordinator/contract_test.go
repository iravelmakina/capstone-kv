package coordinator

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/iravelmakina/capstone-kv/internal/shard"
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
		{"health", "GET", n + "/health", ""}, // 503 until the shards have registered
		{"healthLive", "GET", n + "/health/live", ""},
		{"healthReady", "GET", r + "/health/ready", ""},
		{"healthReady", "GET", n + "/health/ready", ""}, // 503 with a reason
		{"version", "GET", r + "/version", ""},
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
		// Error bodies follow the Error schema; HEAD has no body, and a 503 from /health
		// or /health/ready is a Health or Readiness body (documented that way).
		if code >= 400 && s.method != "HEAD" && s.op != "health" && s.op != "healthReady" {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(body), &e) != nil || e.Error == "" {
				t.Errorf("%s %s -> %d with body %q, want {\"error\": ...}", s.method, s.url, code, body)
			}
		}
	}
	// With API keys and versions (ADR-0027, ADR-0028): 401, 403 and 412 are documented
	// where the coordinator returns them.
	keyedCluster := newCluster(t, 1, true)
	send := keyed(t, keyedCluster)
	k := keyedCluster.coord.URL
	send("POST", k+"/v1/tables", `{"name":"users","partitionKey":"userId"}`, "ops-key")
	send("PUT", k+"/v1/tables/users/items/u1", `{"a":1}`, "ops-key")
	for _, s := range []struct{ op, method, url, body, key string }{
		{"listTables", "GET", k + "/v1/tables", "", ""},                                         // 401
		{"createTable", "POST", k + "/v1/tables", `{"name":"x","partitionKey":"y"}`, "mcp-key"}, // 403
		{"getTable", "GET", k + "/v1/tables/users", "", "nope"},                                 // 401
		{"getItem", "GET", k + "/v1/tables/users/items/u1", "", ""},                             // 401
		{"itemExists", "HEAD", k + "/v1/tables/users/items/u1", "", ""},                         // 401
		{"deleteItem", "DELETE", k + "/v1/tables/users/items/u1", "", "mcp-key"},                // 403
		{"putItem", "PUT", k + "/v1/tables/users/items/u1", `{"a":2}`, ""},                      // 401
	} {
		code, _ := send(s.method, s.url, s.body, s.key)
		if !documented[s.op][code] {
			t.Errorf("%s %s -> %d, not documented for %s", s.method, s.url, code, s.op)
		}
	}
	req, _ := http.NewRequest("PUT", k+"/v1/tables/users/items/u1", strings.NewReader(`{"a":3}`))
	req.Header.Set("Authorization", "Bearer ops-key")
	req.Header.Set("If-None-Match", "*")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 412 || !documented["putItem"][412] {
			t.Errorf("create-only put over an existing item -> %d (412 documented: %v)", resp.StatusCode, documented["putItem"][412])
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

// TestInternalContract does the same for api/internal-openapi.yaml: the coordinator's
// membership endpoints and a shard's key-value endpoints (spec task 10.6). Every operation
// must be exercised and every returned status documented.
func TestInternalContract(t *testing.T) {
	documented := loadOperations(t, "../../api/internal-openapi.yaml")
	c := newCluster(t, 2, true)
	leader := c.leaderOf(t, "shard-1")
	co, sh := c.coord.URL, leader.srv.URL
	follower := c.nodes[c.others(leader.id)[0]].srv.URL
	key := sh + shard.KeyPath(shard.StorageKey("users", "u1", ""))
	missing := sh + shard.KeyPath(shard.StorageKey("users", "nope", ""))
	badKey := sh + "/internal/v1/kv/not*base64"
	big := `{"a":"` + strings.Repeat("x", 1<<20) + `"}`

	type step struct {
		op, method, url, body string
		errorBody             bool // the response must be {"error": ...}
	}
	steps := []step{
		{"replicaHeartbeat", "POST", co + "/internal/v1/replicas", `{"shard":"shard-1","replica":"` + leader.id + `","address":"` + sh + `","epoch":1,"last":{"epoch":0,"lsn":0}}`, false},
		{"replicaHeartbeat", "POST", co + "/internal/v1/replicas", `{"shard":"bad id"}`, true}, // 400
		{"listShards", "GET", co + "/internal/v1/shards", "", false},

		{"shardPut", "PUT", key, `{"name":"Ira"}`, false},
		{"shardPut", "PUT", badKey, `{"a":1}`, true}, // 400
		{"shardPut", "PUT", key, big, true},          // 413
		{"shardGet", "GET", key, "", false},
		{"shardGet", "GET", missing, "", false}, // 404, no body
		{"shardGet", "GET", badKey, "", true},   // 400
		{"shardExists", "HEAD", key, "", false},
		{"shardExists", "HEAD", missing, "", false},
		{"shardExists", "HEAD", badKey, "", false},
		{"shardDelete", "DELETE", key, "", false},
		{"shardDelete", "DELETE", key, "", false}, // 404, nothing written
		{"shardDelete", "DELETE", badKey, "", true},
		{"shardLog", "GET", sh + "/internal/v1/log?replica=x&epoch=0&lsn=0&hash=0&max=10", "", false},
		{"shardLog", "GET", sh + "/internal/v1/log?replica=x&lsn=nope", "", true},                           // 400
		{"shardLog", "GET", follower + "/internal/v1/log?replica=x&epoch=0&lsn=0&hash=0&max=10", "", true},  // 409
		{"shardPut", "PUT", follower + shard.KeyPath(shard.StorageKey("users", "u2", "")), `{"a":1}`, true}, // 409

		{"removeShard", "DELETE", co + "/internal/v1/shards/shard-2", "", false},
		{"removeShard", "DELETE", co + "/internal/v1/shards/shard-2", "", true}, // 404
	}

	exercised := map[string]bool{}
	for _, s := range steps {
		codes, ok := documented[s.op]
		if !ok {
			t.Fatalf("step uses %q, which isn't in internal-openapi.yaml", s.op)
		}
		exercised[s.op] = true
		code, body := call(t, s.method, s.url, s.body)
		if !codes[code] {
			t.Errorf("%s %s -> %d, not documented for %s (documented: %v)", s.method, s.url, code, s.op, keys(codes))
		}
		if s.errorBody {
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
			t.Errorf("operation %s is in internal-openapi.yaml but not exercised by this test", op)
		}
	}
}
