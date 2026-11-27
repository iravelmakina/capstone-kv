package metrics

import (
	"io"
	"math"
	"runtime"
	rm "runtime/metrics"
	"sort"
)

// AddRuntime registers the Go runtime metrics the dashboard shows (ADR-0025): goroutines,
// heap in use, GC cycles and GC pause times, read from runtime/metrics on every scrape.
func (r *Registry) AddRuntime() {
	r.Gauge("go_goroutines", "Goroutines that currently exist.", func() float64 {
		return float64(runtime.NumGoroutine())
	})
	r.Gauge("go_heap_bytes", "Bytes of heap memory occupied by live and not-yet-freed objects.", func() float64 {
		return readUint("/memory/classes/heap/objects:bytes")
	})
	r.Gauge("go_gc_cycles_total", "Completed GC cycles since the process started.", func() float64 {
		return readUint("/gc/cycles/total:gc-cycles")
	})
	r.add(&gcPauses{bounds: []float64{0.00001, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1}})
}

func readUint(name string) float64 {
	s := []rm.Sample{{Name: name}}
	rm.Read(s)
	if s[0].Value.Kind() != rm.KindUint64 {
		return 0
	}
	return float64(s[0].Value.Uint64())
}

// gcPauses turns the runtime's own pause histogram into our fixed buckets on every scrape.
// The runtime keeps finer buckets; each of its buckets is counted under the first of ours
// that its upper edge fits in, so a bound is never too low.
type gcPauses struct{ bounds []float64 }

func (g *gcPauses) write(w io.Writer, cn, cv []string) {
	s := []rm.Sample{{Name: "/gc/pauses:seconds"}}
	rm.Read(s)
	if s[0].Value.Kind() != rm.KindFloat64Histogram {
		return
	}
	h := s[0].Value.Float64Histogram()
	counts := make([]uint64, len(g.bounds)+1)
	var total uint64
	var sum float64
	for i, n := range h.Counts {
		if n == 0 {
			continue
		}
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		j := sort.SearchFloat64s(g.bounds, hi)
		counts[j] += n
		total += n
		mid := hi // estimate of each pause in the bucket, for the sum
		if isFinite(lo) && isFinite(hi) {
			mid = (lo + hi) / 2
		} else if isFinite(lo) {
			mid = lo
		}
		sum += mid * float64(n)
	}
	hist := &Histogram{bounds: g.bounds, counts: counts, sum: sum, count: total}
	vec := &HistogramVec{bounds: g.bounds, v: newVec("go_gc_pause_seconds",
		"Stop-the-world GC pause times (sum estimated from bucket midpoints).", "histogram", nil,
		func() *Histogram { return hist })}
	vec.With()
	vec.write(w, cn, cv)
}

func isFinite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }
