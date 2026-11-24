// Package coordinator is the front door of the store: it serves the public API
// (api/openapi.yaml), keeps the table registry, picks the shard for each item with the
// consistent hashing ring and forwards the request there (ADR-0010 to ADR-0013).
package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

const (
	maxBody     = 1 << 20 // 1 MiB per item, same limit as the shard
	maxKeyValue = 1024    // longest partition or sort key value, per api/openapi.yaml
)

// validName is the name rule for tables and key attributes in api/openapi.yaml.
var validName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Table is a registered table definition. SortKey is empty for partition-key-only tables.
type Table struct {
	Name         string `json:"name"`
	PartitionKey string `json:"partitionKey"`
	SortKey      string `json:"sortKey,omitempty"`
}

// Coordinator holds everything the handlers need.
type Coordinator struct {
	tables  *store.Store // table name -> JSON definition, persisted like shard data
	members *Membership
	client  *http.Client // to the shards: keep-alive connections, short timeout
}

// New builds a coordinator. tables persists the registry; members tracks the shards.
func New(tables *store.Store, members *Membership) *Coordinator {
	return &Coordinator{
		tables:  tables,
		members: members,
		client: &http.Client{
			Timeout: 3 * time.Second,
			// The benchmark runs many requests in parallel; reuse connections to shards
			// instead of opening one per request.
			Transport: &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second},
		},
	}
}

// Handler returns the coordinator's routes: the public API plus the internal shard
// registration endpoints.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", c.health)

	mux.HandleFunc("POST /v1/tables", c.createTable)
	mux.HandleFunc("GET /v1/tables", c.listTables)
	mux.HandleFunc("GET /v1/tables/{table}", c.getTable)

	// Both key shapes go to the same handler; a "GET" pattern also serves HEAD (EXISTS).
	for _, p := range []string{"/v1/tables/{table}/items/{pk}", "/v1/tables/{table}/items/{pk}/{sk}"} {
		mux.HandleFunc("PUT "+p, c.item)
		mux.HandleFunc("GET "+p, c.item)
		mux.HandleFunc("DELETE "+p, c.item)
	}

	mux.HandleFunc("POST /internal/v1/shards", c.registerShard)
	mux.HandleFunc("GET /internal/v1/shards", c.listShards)
	mux.HandleFunc("DELETE /internal/v1/shards/{id}", c.removeShard)
	return mux
}

// health answers 200 only when the coordinator can serve requests, i.e. the expected
// shards have registered; before that 503, so a load balancer or a script checking only
// the status code doesn't send traffic too early. The body has the same fields either way.
func (c *Coordinator) health(w http.ResponseWriter, r *http.Request) {
	ready, n := c.members.Ready()
	code, status := http.StatusOK, "ok"
	if !ready {
		code, status = http.StatusServiceUnavailable, "unavailable"
	}
	httpx.WriteJSON(w, code, map[string]any{
		"status": status, "role": "coordinator", "version": buildinfo.Version, "ready": ready, "shards": n,
	})
}

// --- tables ---------------------------------------------------------------------------

func (c *Coordinator) createTable(w http.ResponseWriter, r *http.Request) {
	var t Table
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields() // the schema says additionalProperties: false
	if err := dec.Decode(&t); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "body must be {name, partitionKey, sortKey?}: "+err.Error())
		return
	}
	if !validName.MatchString(t.Name) || !validName.MatchString(t.PartitionKey) ||
		(t.SortKey != "" && !validName.MatchString(t.SortKey)) {
		httpx.WriteError(w, http.StatusBadRequest, "names must match ^[A-Za-z0-9_.-]{1,128}$")
		return
	}
	if _, exists := c.tables.Get(t.Name); exists {
		httpx.WriteError(w, http.StatusConflict, "table already exists")
		return
	}
	def, _ := json.Marshal(t)
	if err := c.tables.Put(t.Name, def); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, t)
}

func (c *Coordinator) listTables(w http.ResponseWriter, r *http.Request) {
	names := c.tables.Keys()
	sort.Strings(names)
	out := make([]Table, 0, len(names))
	for _, n := range names {
		if t, ok := c.table(n); ok {
			out = append(out, t)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (c *Coordinator) getTable(w http.ResponseWriter, r *http.Request) {
	t, ok := c.table(r.PathValue("table"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "table not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (c *Coordinator) table(name string) (Table, bool) {
	raw, ok := c.tables.Get(name)
	if !ok {
		return Table{}, false
	}
	var t Table
	return t, json.Unmarshal(raw, &t) == nil
}

// --- items ----------------------------------------------------------------------------

// item serves CREATE (PUT), READ (GET), EXISTS (HEAD) and DELETE for both key shapes:
// check readiness, the table and the key, find the owning shard, forward.
func (c *Coordinator) item(w http.ResponseWriter, r *http.Request) {
	if ready, n := c.members.Ready(); !ready {
		httpx.WriteError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("waiting for shards to register (%d so far)", n))
		return
	}
	name, pk, sk := r.PathValue("table"), r.PathValue("pk"), r.PathValue("sk")
	t, ok := c.table(name)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "table not found")
		return
	}
	if (t.SortKey != "") != (sk != "") {
		if t.SortKey != "" {
			httpx.WriteError(w, http.StatusBadRequest, "table "+name+" needs a sort key: /items/{pk}/{sk}")
		} else {
			httpx.WriteError(w, http.StatusBadRequest, "table "+name+" has no sort key: /items/{pk}")
		}
		return
	}
	if err := checkKeyValue(pk); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "partition key: "+err.Error())
		return
	}
	if sk != "" {
		if err := checkKeyValue(sk); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "sort key: "+err.Error())
			return
		}
	}

	var body []byte
	if r.Method == http.MethodPut {
		var err error
		if body, err = readJSONObject(w, r); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// ADR-0012: the shard is chosen by table + partition key; the sort key only names the item.
	id, addr, available, ok := c.members.Owner(ring.RoutingKey(name, pk))
	if !ok || !available {
		httpx.WriteError(w, http.StatusServiceUnavailable, "shard "+id+" is unavailable")
		return
	}
	c.forward(w, r.Method, addr+shard.KeyPath(shard.StorageKey(name, pk, sk)), body, id)
}

// forward sends the item request to the shard and relays its status (and body for READ).
func (c *Coordinator) forward(w http.ResponseWriter, method, url string, body []byte, shardID string) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// Alive by heartbeat but not answering: same honest answer, try later.
		httpx.WriteError(w, http.StatusServiceUnavailable, "shard "+shardID+" did not answer")
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		if method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
		} else {
			httpx.WriteError(w, http.StatusNotFound, "item not found")
		}
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		httpx.WriteError(w, resp.StatusCode, "shard "+shardID+": "+strings.TrimSpace(string(msg)))
	default:
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		if method == http.MethodGet {
			io.Copy(w, resp.Body)
		}
	}
}

// checkKeyValue enforces the key rules from api/openapi.yaml, plus "no zero bytes": the
// shard's storage key uses zero bytes as field separators.
func checkKeyValue(v string) error {
	switch {
	case len(v) > maxKeyValue:
		return errors.New("longer than 1024 bytes")
	case strings.IndexByte(v, 0) >= 0:
		return errors.New("contains a zero byte")
	}
	return nil
}

// readJSONObject reads the PUT body and checks it is one JSON object, as the Item schema
// requires. It returns the body compacted (no whitespace) for storage.
func readJSONObject(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, errors.New("body larger than 1 MiB")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("body must be a JSON object")
	}
	var buf bytes.Buffer
	json.Compact(&buf, raw)
	return buf.Bytes(), nil
}

// --- shard registration (internal) ----------------------------------------------------

func (c *Coordinator) registerShard(w http.ResponseWriter, r *http.Request) {
	var reg shard.Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&reg); err != nil ||
		!validName.MatchString(reg.ID) || !strings.HasPrefix(reg.Address, "http") {
		httpx.WriteError(w, http.StatusBadRequest, "body must be {id, address}")
		return
	}
	c.members.Register(reg.ID, reg.Address)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"registered": reg.ID})
}

func (c *Coordinator) listShards(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, c.members.Status())
}

func (c *Coordinator) removeShard(w http.ResponseWriter, r *http.Request) {
	if !c.members.Remove(r.PathValue("id")) {
		httpx.WriteError(w, http.StatusNotFound, "shard not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
