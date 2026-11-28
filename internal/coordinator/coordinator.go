// Package coordinator is the front door of the store: it serves the public API
// (api/openapi.yaml), keeps the table registry, picks the shard for each item with the
// consistent hashing ring (ADR-0010 to ADR-0013), sends writes to the shard's leader and
// reads to a quorum of its replicas (ADR-0020, ADR-0021).
package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/iravelmakina/capstone-kv/internal/auth"
	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/health"
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
	r       int          // replicas each read asks (R, ADR-0021)
	metrics *coordMetrics
	keys    *auth.Keyring // nil: no API keys required (tests and local runs without keys)
}

// New builds a coordinator. tables persists the registry; members tracks the replica
// sets; r is the read quorum (2 with W = 2, 3 with W = 1).
func New(tables *store.Store, members *Membership, r int) *Coordinator {
	return &Coordinator{
		tables:  tables,
		members: members,
		r:       r,
		client: &http.Client{
			Timeout: 3 * time.Second,
			// The benchmark runs many requests in parallel; reuse connections to shards
			// instead of opening one per request.
			Transport: &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second},
		},
		metrics: newCoordMetrics(members),
	}
}

// Handler returns the coordinator's routes: the public API plus the internal membership
// endpoints.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", c.health)
	// Liveness, readiness and build version (M5), without a key, like /health.
	health.Register(mux, health.NewVersion("coordinator", "", ""), c.members.Readiness)

	// Public routes are observed: request ID, metrics, one log line each (ADR-0025, 0029).
	table := func(*http.Request) string { return "table" }
	mux.HandleFunc("POST /v1/tables", c.observe(table, c.createTable))
	mux.HandleFunc("GET /v1/tables", c.observe(table, c.listTables))
	mux.HandleFunc("GET /v1/tables/{table}", c.observe(table, c.getTable))

	// Both key shapes go to the same handler; a "GET" pattern also serves HEAD (EXISTS).
	for _, p := range []string{"/v1/tables/{table}/items/{pk}", "/v1/tables/{table}/items/{pk}/{sk}"} {
		mux.HandleFunc("PUT "+p, c.observe(itemOp, c.item))
		mux.HandleFunc("GET "+p, c.observe(itemOp, c.item))
		mux.HandleFunc("DELETE "+p, c.observe(itemOp, c.item))
	}

	// Admin (M5): planned leader transfer, admin scope (ADR-0036).
	admin := func(*http.Request) string { return "admin" }
	mux.HandleFunc("POST /v1/admin/shards/{id}/leader", c.observe(admin, c.transferLeader))
	status := func(*http.Request) string { return "status" }
	mux.HandleFunc("GET /v1/admin/cluster", c.observe(status, c.cluster))
	mux.HandleFunc("POST /v1/admin/migrations", c.observe(admin, c.startMigration))
	mux.HandleFunc("GET /v1/admin/migrations/current", c.observe(status, c.currentMigration))
	mux.HandleFunc("DELETE /v1/admin/migrations/current", c.observe(admin, c.abortMigration))

	mux.HandleFunc("POST /internal/v1/replicas", c.heartbeat)
	mux.HandleFunc("GET /internal/v1/shards", c.listShards)
	mux.HandleFunc("DELETE /internal/v1/shards/{id}", c.removeShard)
	return mux
}

// health answers 200 only when the coordinator can serve requests, i.e. the expected
// shards have registered and each has a leader; before that 503, so a load balancer or a script checking only
// the status code doesn't send traffic too early. The body has the same fields either way.
func (c *Coordinator) health(w http.ResponseWriter, r *http.Request) {
	ready, n := c.members.Ready()
	code, status := http.StatusOK, "ok"
	if !ready {
		code, status = http.StatusServiceUnavailable, "unavailable"
	}
	failures := map[string]map[string]uint64{}
	for _, s := range c.members.Status() {
		failures[s.ID] = map[string]uint64{"unavailable503": s.Unavailable, "notCommitted504": s.NotCommitted}
	}
	httpx.WriteJSON(w, code, map[string]any{
		"status": status, "role": "coordinator", "version": buildinfo.Version, "ready": ready, "shards": n,
		"failures": failures,
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
	storageKey := shard.StorageKey(name, pk, sk)
	routing, path := ring.RoutingKey(name, pk), shard.KeyPath(storageKey)
	ri := info(r)
	ri.keyHash = strconv.FormatUint(xxhash.Sum64String(storageKey), 16)
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		c.write(w, ri, r.Header, r.Method, routing, path, body)
	} else {
		c.read(w, ri, r.Method, routing, path)
	}
}

// write sends a put or delete to the shard's leader and relays the answer (ADR-0021).
// During a planned leader transfer (ADR-0036) it waits out the shard's write hold first,
// and during a migration's cutover (ADR-0033) the hold on the moving key's source; a write
// the leader refused with 409 (stepping down, or the key frozen as "moved") waits for the
// holds too and goes once more to whoever owns and leads then: the refused write was never
// appended, so sending it again can't apply it twice.
func (c *Coordinator) write(w http.ResponseWriter, ri *reqInfo, in http.Header, method string, routing []byte, path string, body []byte) {
	ctx := context.Background()
	var resp *http.Response
	var id string
	for attempt := 0; ; attempt++ {
		var addr string
		var ok bool
		id, addr, ok = c.members.WriteTarget(routing)
		ri.shard = id
		if held := c.members.WaitHold(ctx, id); held > 0 {
			c.metrics.hold.With(id).Observe(held.Seconds())
			id, addr, ok = c.members.WriteTarget(routing)
		}
		if src, held := c.members.WaitCutover(ctx, routing); held > 0 { // a migration's cutover (ADR-0033)
			c.metrics.hold.With(src).Observe(held.Seconds())
			id, addr, ok = c.members.WriteTarget(routing)
			ri.shard = id
		}
		if !ok {
			c.fail(w, id, http.StatusServiceUnavailable, "shard "+id+" has no live leader")
			return
		}
		req, _ := http.NewRequest(method, addr+path, bytes.NewReader(body))
		req.Header.Set(httpx.HeaderRequestID, ri.id)
		for _, h := range versionRequestHeaders { // a retry's stamp, conditions (ADR-0028)
			if v := in.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		var err error
		resp, err = c.client.Do(req)
		if err != nil {
			// Alive by heartbeat but not answering: same honest answer, try later.
			c.fail(w, id, http.StatusServiceUnavailable, "leader of "+id+" did not answer")
			return
		}
		if resp.StatusCode != http.StatusConflict || attempt > 0 {
			break
		}
		// 409: not (or no longer) the leader, or "moved" (the key's source froze it in a
		// cutover). Retry once if leadership or ownership is moving or moved.
		held := c.members.WaitHold(ctx, id)
		_, cut := c.members.WaitCutover(ctx, routing)
		_, now, _ := c.members.WriteTarget(routing)
		if held == 0 && cut == 0 && now == addr {
			break // nothing changed: answer as before
		}
		resp.Body.Close()
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	for _, h := range versionResponseHeaders { // the write's stamp, superseded, version
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		w.WriteHeader(resp.StatusCode)
	case http.StatusNotFound:
		httpx.WriteError(w, http.StatusNotFound, "item not found")
	case http.StatusConflict: // leadership moved between our lookup and its answer
		c.fail(w, id, http.StatusServiceUnavailable, "leader of "+id+" changed; retry")
	case http.StatusGatewayTimeout:
		c.fail(w, id, http.StatusGatewayTimeout, "shard "+id+": "+errorText(msg))
	default:
		httpx.WriteError(w, resp.StatusCode, "shard "+id+": "+errorText(msg))
	}
}

// answer is one replica's reply to a read.
type answer struct {
	found   bool
	version store.Version // zero when the replica never had the key
	stamp   string        // the record's HLC stamp (ADR-0028)
	body    []byte
}

// Headers a write passes to the leader, and those its answer passes back (ADR-0028).
var (
	versionRequestHeaders  = []string{shard.HeaderStamp, "If-Match", "If-None-Match"}
	versionResponseHeaders = []string{shard.HeaderStamp, shard.HeaderSuperseded, "ETag"}
)

// read asks R live replicas of the shard (rotating, ADR-0021) and returns the answer
// with the newest version; a tombstone or "never had it" is 404. A replica that doesn't
// answer is replaced by the next live one; fewer than R answers is 503.
func (c *Coordinator) read(w http.ResponseWriter, ri *reqInfo, method string, routing []byte, path string) {
	id, addrs := c.members.ReadTargets(routing)
	ri.shard = id
	var answers []answer
	for len(answers) < c.r && len(addrs) > 0 {
		n := min(c.r-len(answers), len(addrs))
		batch := addrs[:n]
		addrs = addrs[n:]
		got := make([]*answer, n)
		took := make([]time.Duration, n)
		var wg sync.WaitGroup
		for i, addr := range batch {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				got[i] = c.ask(ri.id, method, addr+path)
				took[i] = time.Since(start)
			}()
		}
		wg.Wait()
		for i, addr := range batch {
			ri.replicas = append(ri.replicas, fmt.Sprintf("%s=%.1fms", nodeOf(addr)+portOf(addr), float64(took[i].Microseconds())/1000))
		}
		for _, a := range got {
			if a != nil {
				answers = append(answers, *a)
			}
		}
	}
	if len(answers) < c.r {
		c.fail(w, id, http.StatusServiceUnavailable,
			fmt.Sprintf("shard %s: %d of %d replicas answered", id, len(answers), c.r))
		return
	}
	newest := answers[0]
	for _, a := range answers[1:] {
		if newest.version.Less(a.version) {
			newest = a
		}
	}
	switch {
	case !newest.found && method == http.MethodHead:
		w.WriteHeader(http.StatusNotFound)
	case !newest.found:
		httpx.WriteError(w, http.StatusNotFound, "item not found")
	default:
		w.Header().Set("Content-Type", "application/json")
		if newest.stamp != "" {
			w.Header().Set(shard.HeaderStamp, newest.stamp)
			w.Header().Set("ETag", `"`+newest.stamp+`"`)
		}
		w.WriteHeader(http.StatusOK)
		if method == http.MethodGet {
			w.Write(newest.body)
		}
	}
}

// ask reads one replica; nil means it didn't give a usable answer.
func (c *Coordinator) ask(requestID, method, url string) *answer {
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set(httpx.HeaderRequestID, requestID)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil
	}
	a := &answer{found: resp.StatusCode == http.StatusOK}
	a.version.Epoch, _ = strconv.ParseUint(resp.Header.Get(shard.HeaderEpoch), 10, 64)
	a.version.LSN, _ = strconv.ParseUint(resp.Header.Get(shard.HeaderLSN), 10, 64)
	a.stamp = resp.Header.Get(shard.HeaderStamp)
	if a.found && method == http.MethodGet {
		if a.body, err = io.ReadAll(resp.Body); err != nil {
			return nil
		}
	}
	return a
}

// fail answers an error and counts 503s and 504s per shard.
func (c *Coordinator) fail(w http.ResponseWriter, shardID string, status int, msg string) {
	c.members.count(shardID, status)
	httpx.WriteError(w, status, msg)
}

// errorText is a replica's {"error": "..."} message, or its raw body.
func errorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
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

// --- membership (internal) ------------------------------------------------------------

// heartbeat records a replica's report and answers with its set's epoch and leader.
func (c *Coordinator) heartbeat(w http.ResponseWriter, r *http.Request) {
	var hb shard.Heartbeat
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&hb); err != nil ||
		!validName.MatchString(hb.Shard) || !validName.MatchString(hb.Replica) ||
		!strings.HasPrefix(hb.Address, "http") {
		httpx.WriteError(w, http.StatusBadRequest, "body must be {shard, replica, address, epoch, last}")
		return
	}
	answer, err := c.members.Heartbeat(hb)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, answer)
}

// transferLeader is POST /v1/admin/shards/{id}/leader {"to": "<replica>"}: a planned
// leader transfer (ADR-0036). 200 with the new epoch and leader; 404 for an unknown shard
// or replica; 409 when it can't happen or was abandoned (the old leader still leads).
func (c *Coordinator) transferLeader(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To string `json:"to"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || !validName.MatchString(body.To) {
		httpx.WriteError(w, http.StatusBadRequest, `body must be {"to": "<replica ID>"}`)
		return
	}
	id := r.PathValue("id")
	info(r).shard = id
	ans, err := c.members.Transfer(r.Context(), id, body.To)
	var te *TransferError
	switch {
	case errors.As(err, &te):
		httpx.WriteError(w, te.Status, te.Msg)
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"shard": id, "leader": ans.Leader, "epoch": ans.Epoch})
	}
}

func (c *Coordinator) listShards(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, c.members.Status())
}

func (c *Coordinator) removeShard(w http.ResponseWriter, r *http.Request) {
	var se *StatusError
	switch err := c.members.Remove(r.PathValue("id")); {
	case errors.As(err, &se):
		httpx.WriteError(w, se.Status, se.Msg)
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
