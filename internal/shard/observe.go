package shard

import (
	"net/http"
	"sort"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/httpx"
	"github.com/iravelmakina/capstone-kv/internal/metrics"
)

// replicaMetrics are one replica's series; every one carries its shard and replica as
// constant labels (ADR-0025).
type replicaMetrics struct {
	reg        *metrics.Registry
	latency    *metrics.HistogramVec // kv_replica_request_duration_seconds{op}
	errors     *metrics.CounterVec   // kv_replica_errors_total{op}
	commitWait *metrics.HistogramVec // kv_commit_wait_seconds (leader)
}

func (r *Replica) initMetrics() {
	reg := metrics.New("shard", r.cfg.Shard, "replica", r.cfg.Replica)
	r.metrics = &replicaMetrics{
		reg: reg,
		latency: reg.Histogram("kv_replica_request_duration_seconds",
			"Item requests served by this replica, by operation.", metrics.LatencyBuckets, "op"),
		errors: reg.Counter("kv_replica_errors_total",
			"Item requests this replica answered with a 5xx status, by operation.", "op"),
		commitWait: reg.Histogram("kv_commit_wait_seconds",
			"Leader: time a write waited for a follower before it committed or timed out.", metrics.LatencyBuckets),
	}
	reg.GaugeFunc("kv_replica_role", "1 for the role this replica has now.", []string{"role"}, func() []metrics.Sample {
		return []metrics.Sample{{Labels: []string{r.Status().Role}, Value: 1}}
	})
	reg.Gauge("kv_store_keys", "Keys in this replica's map, tombstones not counted.", func() float64 {
		return float64(r.st.Len())
	})
	reg.Gauge("kv_log_bytes", "Size of this replica's log on disk.", func() float64 {
		return float64(r.st.Size())
	})
	reg.CounterFunc("kv_replica_reads_total", "Reads (GET and HEAD) served by this replica.", func() float64 {
		return float64(r.reads.Load())
	})
	reg.GaugeFunc("kv_follower_acked_lsn", "Leader: each follower's last durable LSN, from its pulls.",
		[]string{"follower"}, func() []metrics.Sample { return r.followerSamples(false) })
	reg.GaugeFunc("kv_follower_lag_seconds",
		"Leader: seconds since each follower last held the leader's last record (0 when caught up).",
		[]string{"follower"}, func() []metrics.Sample { return r.followerSamples(true) })
	reg.AddRuntime()
	reg.AddBuildInfo(buildinfo.Version, "shard")
}

// followerSamples reports, on a leader, each follower that has pulled in this epoch: its
// acked LSN, or how long it has been behind. Measured here rather than on the follower, so
// it keeps growing when a follower is wedged or cut off and can't report.
func (r *Replica) followerSamples(lagSeconds bool) []metrics.Sample {
	last, _ := r.st.Last()
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isLeaderLocked() {
		return nil
	}
	ids := make([]string, 0, len(r.acked))
	for f := range r.acked {
		ids = append(ids, f)
	}
	sort.Strings(ids)
	out := make([]metrics.Sample, 0, len(ids))
	for _, f := range ids {
		v := float64(r.acked[f])
		if lagSeconds {
			v = 0
			if r.acked[f] < last.LSN {
				since := r.caughtUp[f]
				if since.IsZero() {
					since = r.leaderSince
				}
				v = now.Sub(since).Seconds()
			}
		}
		out = append(out, metrics.Sample{Labels: []string{f}, Value: v})
	}
	return out
}

// observe wraps an item route: latency and 5xx counts by operation. The log endpoint
// isn't timed (a long-poll lasts as long as it waits), only its failures are counted.
func (r *Replica) observe(op string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		rec := &httpx.StatusRecorder{ResponseWriter: w}
		name := op
		if op == "read" && req.Method == http.MethodHead {
			name = "exists"
		}
		h(rec, req)
		if name != "log" {
			r.metrics.latency.With(name).Observe(time.Since(start).Seconds())
		}
		if rec.Code() >= 500 {
			r.metrics.errors.With(name).Inc()
		}
	}
}

// MetricsHandler serves the replica's metrics, on the metrics port (ADR-0025).
func (r *Replica) MetricsHandler() http.Handler { return r.metrics.reg.Handler() }
