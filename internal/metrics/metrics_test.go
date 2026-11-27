package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func scrape(r *Registry) string {
	var b bytes.Buffer
	r.Write(&b)
	return b.String()
}

func TestCounterWithLabels(t *testing.T) {
	r := New()
	c := r.Counter("kv_requests_total", "Requests.", "op", "code")
	c.With("read", "200").Inc()
	c.With("read", "200").Inc()
	c.With("write", "504").Add(3)
	got := scrape(r)
	for _, want := range []string{
		"# HELP kv_requests_total Requests.\n# TYPE kv_requests_total counter\n",
		`kv_requests_total{op="read",code="200"} 2` + "\n",
		`kv_requests_total{op="write",code="504"} 3` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Bucket counts are cumulative, a value equal to a bound falls into that bound's bucket
// (le = "less than or equal"), and values above the last bound only reach +Inf.
func TestHistogramBuckets(t *testing.T) {
	r := New()
	h := r.Histogram("lat_seconds", "Latency.", []float64{0.01, 0.1, 1}, "op")
	for _, v := range []float64{0.005, 0.01, 0.05, 0.5, 2} {
		h.With("read").Observe(v)
	}
	got := scrape(r)
	for _, want := range []string{
		`lat_seconds_bucket{op="read",le="0.01"} 2`,
		`lat_seconds_bucket{op="read",le="0.1"} 3`,
		`lat_seconds_bucket{op="read",le="1"} 4`,
		`lat_seconds_bucket{op="read",le="+Inf"} 5`,
		`lat_seconds_sum{op="read"} 2.565`,
		`lat_seconds_count{op="read"} 5`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestSLOBoundsAreBucketEdges(t *testing.T) {
	has := map[float64]bool{}
	for _, b := range LatencyBuckets {
		has[b] = true
	}
	if !has[0.015] || !has[0.06] {
		t.Fatalf("buckets %v must include the SLO thresholds 0.015 and 0.06 (ADR-0026)", LatencyBuckets)
	}
	for i := 1; i < len(LatencyBuckets); i++ {
		if LatencyBuckets[i] <= LatencyBuckets[i-1] {
			t.Fatalf("buckets must ascend: %v", LatencyBuckets)
		}
	}
}

func TestLabelValuesEscaped(t *testing.T) {
	r := New()
	r.Counter("x_total", "X.", "v").With("a\"b\\c\nd").Inc()
	if got := scrape(r); !strings.Contains(got, `x_total{v="a\"b\\c\nd"} 1`) {
		t.Fatalf("label not escaped:\n%s", got)
	}
}

func TestGaugeFuncSortedAndEmptyFamiliesSkipped(t *testing.T) {
	r := New()
	r.Counter("unused_total", "Never used.")
	r.GaugeFunc("kv_replica_up", "Up.", []string{"replica"}, func() []Sample {
		return []Sample{{Labels: []string{"1b"}, Value: 0}, {Labels: []string{"1a"}, Value: 1}}
	})
	got := scrape(r)
	if strings.Contains(got, "unused_total") {
		t.Errorf("a family with no series should not be written:\n%s", got)
	}
	a, b := strings.Index(got, `replica="1a"`), strings.Index(got, `replica="1b"`)
	if a < 0 || b < 0 || a > b {
		t.Errorf("gauge samples not sorted:\n%s", got)
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a missing label value")
		}
	}()
	New().Counter("x_total", "X.", "a", "b").With("only-one")
}

func TestConcurrentObserve(t *testing.T) {
	r := New()
	h := r.Histogram("c_seconds", "C.", LatencyBuckets)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				h.With().Observe(0.003)
			}
		}()
	}
	wg.Wait()
	if got := scrape(r); !strings.Contains(got, "c_seconds_count 8000\n") {
		t.Fatalf("lost observations:\n%s", got)
	}
}

func TestConstLabelsOnEverySeries(t *testing.T) {
	r := New("shard", "shard-1", "replica", "1a")
	r.Counter("e_total", "E.", "op").With("write").Inc()
	r.Histogram("d_seconds", "D.", []float64{1}).With().Observe(0.5)
	r.Gauge("g", "G.", func() float64 { return 3 })
	got := scrape(r)
	for _, want := range []string{
		`e_total{shard="shard-1",replica="1a",op="write"} 1`,
		`d_seconds_bucket{shard="shard-1",replica="1a",le="1"} 1`,
		`d_seconds_count{shard="shard-1",replica="1a"} 1`,
		`g{shard="shard-1",replica="1a"} 3`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRuntimeMetrics(t *testing.T) {
	r := New()
	r.AddRuntime()
	got := scrape(r)
	for _, want := range []string{"go_goroutines ", "go_heap_bytes ", "go_gc_cycles_total ", "# TYPE go_gc_pause_seconds histogram"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}
