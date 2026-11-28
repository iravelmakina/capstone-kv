package coordinator

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/auth"
	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/metrics"
)

// reqInfo is what the handlers learn about a request while serving it, read back by
// observe for the metrics and the log line (ADR-0025, ADR-0029).
type reqInfo struct {
	id        string
	principal string   // the API key's name; "-" before authentication
	shard     string   // the shard the item belongs to, once routed
	keyHash   string   // a hash of the storage key, never the key itself
	replicas  []string // reads: "1a=2.1ms" per replica asked
}

type infoKey struct{}

// info returns the request's reqInfo; requests that didn't pass through observe get an
// empty one, so handlers never have to check.
func info(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(infoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{principal: "-"}
}

// coordMetrics are the coordinator's own series.
type coordMetrics struct {
	reg      *metrics.Registry
	requests *metrics.CounterVec   // kv_requests_total{op,code,shard,principal}
	latency  *metrics.HistogramVec // kv_request_duration_seconds{op,shard}
	hold     *metrics.HistogramVec // kv_write_hold_seconds{shard}
}

func newCoordMetrics(m *Membership) *coordMetrics {
	reg := metrics.New()
	cm := &coordMetrics{
		reg: reg,
		requests: reg.Counter("kv_requests_total",
			"Public API requests by operation, status code, shard and API key name.", "op", "code", "shard", "principal"),
		latency: reg.Histogram("kv_request_duration_seconds",
			"Public API request latency at the coordinator, by operation and shard.", metrics.LatencyBuckets, "op", "shard"),
		hold: reg.Histogram("kv_write_hold_seconds",
			"Time writes waited on a shard's write hold (planned leader transfer, ADR-0036).", metrics.LatencyBuckets, "shard"),
	}
	reg.GaugeFunc("kv_replica_up", "1 if the replica has sent a heartbeat within the timeout.",
		[]string{"shard", "replica", "node"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range m.Status() {
				for _, r := range s.Replicas {
					out = append(out, metrics.Sample{Labels: []string{s.ID, r.ID, nodeOf(r.Address)}, Value: boolValue(r.Available)})
				}
			}
			return out
		})
	reg.GaugeFunc("kv_shard_has_leader", "1 if the shard has a leader that is alive.",
		[]string{"shard"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range m.Status() {
				alive := false
				for _, r := range s.Replicas {
					alive = alive || (r.ID == s.Leader && r.Available)
				}
				out = append(out, metrics.Sample{Labels: []string{s.ID}, Value: boolValue(alive)})
			}
			return out
		})
	reg.GaugeFunc("kv_shard_leader_epoch", "The shard's current leader epoch; a step is a leader change.",
		[]string{"shard", "leader"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range m.Status() {
				out = append(out, metrics.Sample{Labels: []string{s.ID, s.Leader}, Value: float64(s.Epoch)})
			}
			return out
		})
	reg.GaugeFunc("kv_replica_last_lsn", "The last LSN each replica reported in its heartbeat.",
		[]string{"shard", "replica"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range m.Status() {
				for _, r := range s.Replicas {
					out = append(out, metrics.Sample{Labels: []string{s.ID, r.ID}, Value: float64(r.Last.LSN)})
				}
			}
			return out
		})
	reg.GaugeFunc("kv_shard_state", "1 for each replica set's state: pending, active, leaving or retired (ADR-0033).",
		[]string{"shard", "state"}, func() []metrics.Sample {
			var out []metrics.Sample
			for _, s := range m.Status() {
				out = append(out, metrics.Sample{Labels: []string{s.ID, s.State}, Value: 1})
			}
			return out
		})
	reg.Gauge("kv_ring_shards", "Replica sets on the hash ring (active or leaving): the store's capacity in sets.",
		func() float64 {
			n := 0
			for _, s := range m.Status() {
				if onRing(s.State) {
					n++
				}
			}
			return float64(n)
		})
	reg.Gauge("kv_migration_running", "1 while a migration (a ring change, ADR-0033) is running.", func() float64 {
		return boolValue(m.CurrentMigration().Running())
	})
	reg.GaugeFunc("kv_migration_phase", "1 for the current (or last) migration's phase.", []string{"migration", "phase"},
		func() []metrics.Sample {
			if mg := m.CurrentMigration(); mg != nil {
				return []metrics.Sample{{Labels: []string{mg.ID, mg.Phase}, Value: 1}}
			}
			return nil
		})
	reg.CounterFunc("kv_migration_progress_total",
		"Records copied or scanned and steps completed by the migration runner; flat while running = stalled.",
		func() float64 { return float64(m.migProgress.Load()) })
	sourceSamples := func(value func(*SourceProgress) float64) func() []metrics.Sample {
		return func() []metrics.Sample {
			mg := m.CurrentMigration()
			if !mg.Running() {
				return nil
			}
			var out []metrics.Sample
			for src, p := range mg.Sources {
				out = append(out, metrics.Sample{Labels: []string{src}, Value: value(p)})
			}
			return out
		}
	}
	reg.GaugeFunc("kv_migration_records_copied", "Running migration: records applied on new owners, per source.",
		[]string{"source"}, sourceSamples(func(p *SourceProgress) float64 { return float64(p.Copied) }))
	reg.GaugeFunc("kv_migration_records_left", "Running migration: committed records the source still has to send (tail).",
		[]string{"source"}, sourceSamples(func(p *SourceProgress) float64 { return float64(max(p.Left, 0)) }))
	reg.Gauge("kv_nodes", "Nodes (instances) the coordinator knows a live replica on: capacity in machines.", func() float64 {
		nodes := map[string]bool{}
		for _, s := range m.Status() {
			for _, r := range s.Replicas {
				if r.Available {
					nodes[nodeOf(r.Address)] = true
				}
			}
		}
		return float64(len(nodes))
	})
	reg.GaugeFunc("kv_node_leaders", "Shard leaders running on each node (ADR-0036: at most one where possible).",
		[]string{"node"}, func() []metrics.Sample {
			perNode, _ := m.LeaderPlacement()
			out := make([]metrics.Sample, 0, len(perNode))
			for node, n := range perNode {
				out = append(out, metrics.Sample{Labels: []string{node}, Value: float64(n)})
			}
			return out
		})
	reg.Gauge("kv_leaders_colocated",
		"Shards whose leader shares a node with another leader while a live follower is on a leaderless node.",
		func() float64 {
			_, colocated := m.LeaderPlacement()
			return float64(len(colocated))
		})
	reg.AddRuntime()
	reg.AddBuildInfo(buildinfo.Version, "coordinator")
	return cm
}

// nodeOf is the host part of a replica's address: replicas on the same instance share it,
// which is how alerts tell a node down from one replica down (ADR-0020 placement).
func nodeOf(address string) string {
	if u, err := url.Parse(address); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return address
}

// portOf is ":8081" for a replica address, so a log line tells two replicas on one node apart.
func portOf(address string) string {
	if u, err := url.Parse(address); err == nil && u.Port() != "" {
		return ":" + u.Port()
	}
	return ""
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// itemOp names an item request's operation for metrics and logs.
func itemOp(r *http.Request) string {
	switch r.Method {
	case http.MethodPut:
		return "write"
	case http.MethodDelete:
		return "delete"
	case http.MethodHead:
		return "exists"
	}
	return "read"
}

// UseKeys makes every public route require an API key with the right scope (ADR-0027).
func (c *Coordinator) UseKeys(k *auth.Keyring) { c.keys = k }

// scopeOf is the scope an operation needs.
func scopeOf(op, method string) string {
	switch {
	case op == "write":
		return auth.Write
	case op == "delete":
		return auth.Delete
	case op == "table" && method == http.MethodPost:
		return auth.Admin
	case op == "admin": // M5 admin actions: migrations, leader transfer
		return auth.Admin
	case op == "status": // M5 admin reads: cluster state, migration
		return auth.Status
	}
	return auth.Read
}

// observe wraps a public route: it gives the request an ID (returned to the client),
// checks its API key and scope, and afterwards counts it, records its latency and logs one
// JSON line about it, naming the key (never its value).
func (c *Coordinator) observe(op func(*http.Request) string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{id: httpx.RequestID(r), principal: "-"}
		w.Header().Set(httpx.HeaderRequestID, ri.id)
		rec := &httpx.StatusRecorder{ResponseWriter: w}
		name := op(r)
		if c.authorize(rec, r, ri, scopeOf(name, r.Method)) {
			h(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, ri)))
		}

		code, took := rec.Code(), time.Since(start)
		c.metrics.requests.With(name, strconv.Itoa(code), ri.shard, ri.principal).Inc()
		c.metrics.latency.With(name, ri.shard).Observe(took.Seconds())
		attrs := []any{"request_id", ri.id, "principal", ri.principal, "op", name, "status", code,
			"duration_ms", float64(took.Microseconds()) / 1000}
		if ri.shard != "" {
			attrs = append(attrs, "shard", ri.shard, "key_hash", ri.keyHash)
		}
		if len(ri.replicas) > 0 {
			attrs = append(attrs, "replicas", strings.Join(ri.replicas, ","))
		}
		slog.Info("request", attrs...)
	}
}

// authorize checks the request's key, records its name as the principal, and answers 401
// or 403 itself when the request may not go on.
func (c *Coordinator) authorize(w http.ResponseWriter, r *http.Request, ri *reqInfo, scope string) bool {
	if c.keys == nil {
		return true
	}
	key, ok := c.keys.Check(auth.Bearer(r.Header.Get("Authorization")))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="capstone-kv"`)
		httpx.WriteError(w, http.StatusUnauthorized, "a valid API key is required: Authorization: Bearer <key>")
		return false
	}
	ri.principal = key.Name
	if !key.Allows(scope) {
		httpx.WriteError(w, http.StatusForbidden, "this key may not "+scope)
		return false
	}
	return true
}

// MetricsHandler serves the coordinator's metrics, on the metrics port (ADR-0025).
func (c *Coordinator) MetricsHandler() http.Handler { return c.metrics.reg.Handler() }
