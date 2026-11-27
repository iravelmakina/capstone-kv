// Package metrics writes counters, gauges and histograms in the Prometheus text exposition
// format, with the standard library only (ADR-0025, ADR-0009). It does just what the
// store needs: labelled series with fixed label names, cumulative histogram buckets, and
// gauges computed when the endpoint is scraped.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// LatencyBuckets are the histogram bounds in seconds. They include both SLO thresholds,
// 15 ms and 60 ms (ADR-0026), so SLO checks don't interpolate inside a bucket.
var LatencyBuckets = []float64{0.001, 0.002, 0.004, 0.008, 0.015, 0.03, 0.06, 0.12, 0.25, 0.5, 1, 2, 5}

// Registry holds every metric family of one process, in registration order, and labels
// that every series of the process carries (e.g. shard and replica).
type Registry struct {
	mu          sync.Mutex
	families    []family
	constNames  []string
	constValues []string
}

// family is anything that can write itself in the text format; cn and cv are the
// registry's constant label names and values, written first on every series.
type family interface {
	write(w io.Writer, cn, cv []string)
}

// New returns an empty registry. constLabels are name, value pairs added to every series.
func New(constLabels ...string) *Registry {
	r := &Registry{}
	for i := 0; i+1 < len(constLabels); i += 2 {
		r.constNames = append(r.constNames, constLabels[i])
		r.constValues = append(r.constValues, constLabels[i+1])
	}
	return r
}

func (r *Registry) add(f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.families = append(r.families, f)
}

// Write writes every family in the Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	fams := append([]family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range fams {
		f.write(w, r.constNames, r.constValues)
	}
}

// Handler serves the registry on GET.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// --- series bookkeeping shared by the vectors --------------------------------------------

// vec maps label values to one series each. The label names are fixed at registration.
type vec[S any] struct {
	name, help, kind string
	labels           []string
	mu               sync.Mutex
	series           map[string]*S
	values           map[string][]string // series key -> its label values, for writing
	fresh            func() *S
}

func newVec[S any](name, help, kind string, labels []string, fresh func() *S) *vec[S] {
	return &vec[S]{name: name, help: help, kind: kind, labels: labels,
		series: map[string]*S{}, values: map[string][]string{}, fresh: fresh}
}

// with returns the series for the label values, creating it on first use.
func (v *vec[S]) with(values []string) *S {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", v.name, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.series[key]
	if !ok {
		s = v.fresh()
		v.series[key] = s
		v.values[key] = append([]string(nil), values...)
	}
	return s
}

// sorted returns the series keys in a stable order, so scrapes are reproducible.
func (v *vec[S]) sorted() ([]string, map[string]*S, map[string][]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	keys := make([]string, 0, len(v.series))
	series := make(map[string]*S, len(v.series))
	values := make(map[string][]string, len(v.series))
	for k, s := range v.series {
		keys = append(keys, k)
		series[k] = s
		values[k] = v.values[k]
	}
	sort.Strings(keys)
	return keys, series, values
}

func (v *vec[S]) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.name, v.help, v.name, v.kind)
}

// labelString renders {a="x",b="y"} plus any extra pairs (e.g. le for buckets).
func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	n := 0
	put := func(k, v string) {
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escape(v))
		b.WriteByte('"')
		n++
	}
	for i, k := range names {
		put(k, values[i])
	}
	for i := 0; i+1 < len(extra); i += 2 {
		put(extra[i], extra[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// join returns a followed by b in a new slice (never aliasing either).
func join(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// escape applies the text format's rules for label values: backslash, quote, newline.
func escape(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// --- counters -----------------------------------------------------------------------------

// Counter only goes up.
type Counter struct{ n atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.n.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.n.Add(n) }

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.n.Load() }

// CounterVec is a counter family with labels.
type CounterVec struct{ v *vec[Counter] }

// Counter registers a counter family. Names should end in _total.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{newVec(name, help, "counter", labels, func() *Counter { return &Counter{} })}
	r.add(c)
	return c
}

// With returns the counter for these label values.
func (c *CounterVec) With(values ...string) *Counter { return c.v.with(values) }

func (c *CounterVec) write(w io.Writer, cn, cv []string) {
	keys, series, values := c.v.sorted()
	if len(keys) == 0 {
		return
	}
	c.v.header(w)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %d\n", c.v.name, labelString(join(cn, c.v.labels), join(cv, values[k])), series[k].Value())
	}
}

// --- histograms ---------------------------------------------------------------------------

// Histogram counts observations into fixed buckets.
type Histogram struct {
	bounds []float64
	mu     sync.Mutex
	counts []uint64 // per bucket, not cumulative; the last one is +Inf
	sum    float64
	count  uint64
}

// Observe records one value (seconds, for latency).
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

// snapshot returns cumulative bucket counts, the sum and the count.
func (h *Histogram) snapshot() ([]uint64, float64, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cum := make([]uint64, len(h.counts))
	var run uint64
	for i, n := range h.counts {
		run += n
		cum[i] = run
	}
	return cum, h.sum, h.count
}

// HistogramVec is a histogram family with labels.
type HistogramVec struct {
	v      *vec[Histogram]
	bounds []float64
}

// Histogram registers a histogram family with the given bucket bounds (ascending).
func (r *Registry) Histogram(name, help string, bounds []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), bounds...)
	h := &HistogramVec{bounds: b}
	h.v = newVec(name, help, "histogram", labels, func() *Histogram {
		return &Histogram{bounds: b, counts: make([]uint64, len(b)+1)}
	})
	r.add(h)
	return h
}

// With returns the histogram for these label values.
func (h *HistogramVec) With(values ...string) *Histogram { return h.v.with(values) }

func (h *HistogramVec) write(w io.Writer, cn, cv []string) {
	keys, series, values := h.v.sorted()
	if len(keys) == 0 {
		return
	}
	h.v.header(w)
	for _, k := range keys {
		cum, sum, count := series[k].snapshot()
		names, vals := join(cn, h.v.labels), join(cv, values[k])
		for i, bound := range h.bounds {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.v.name, labelString(names, vals, "le", formatFloat(bound)), cum[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.v.name, labelString(names, vals, "le", "+Inf"), cum[len(cum)-1])
		fmt.Fprintf(w, "%s_sum%s %s\n", h.v.name, labelString(names, vals), formatFloat(sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.v.name, labelString(names, vals), count)
	}
}

// --- gauges computed at scrape time -------------------------------------------------------

// Sample is one gauge value with its label values.
type Sample struct {
	Labels []string
	Value  float64
}

// gaugeFunc calls a function on every scrape: for values another part of the program
// already knows (replicas up, keys in the store), so nothing has to be kept in step.
type gaugeFunc struct {
	name, help string
	labels     []string
	collect    func() []Sample
}

// GaugeFunc registers a gauge family whose samples collect returns on every scrape.
func (r *Registry) GaugeFunc(name, help string, labels []string, collect func() []Sample) {
	r.add(&gaugeFunc{name: name, help: help, labels: labels, collect: collect})
}

func (g *gaugeFunc) write(w io.Writer, cn, cv []string) {
	samples := g.collect()
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool {
		return strings.Join(samples[i].Labels, "\xff") < strings.Join(samples[j].Labels, "\xff")
	})
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	for _, s := range samples {
		fmt.Fprintf(w, "%s%s %s\n", g.name, labelString(join(cn, g.labels), join(cv, s.Labels)), formatFloat(s.Value))
	}
}

// Gauge is shorthand for a gauge without labels.
func (r *Registry) Gauge(name, help string, value func() float64) {
	r.GaugeFunc(name, help, nil, func() []Sample { return []Sample{{Value: value()}} })
}

// CounterFunc registers a counter family whose value another part of the program already
// counts (e.g. reads served, kept for the health endpoint).
func (r *Registry) CounterFunc(name, help string, value func() float64) {
	r.add(&counterFunc{name: name, help: help, value: value})
}

type counterFunc struct {
	name, help string
	value      func() float64
}

func (c *counterFunc) write(w io.Writer, cn, cv []string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s%s %s\n", c.name, c.help, c.name, c.name, labelString(cn, cv), formatFloat(c.value()))
}
