package ring

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestDistribution is the measurement behind ADR-0011: it hashes 1,000,000 benchmark-style
// keys onto rings of 2, 3 and 5 shards at several virtual-node counts and reports how far
// the busiest and quietest shards are from the mean. Run with -v to print the table that
// goes into docs/m2/ring-distribution.md.
//
// Acceptance (ADR-0011): at DefaultVirtualNodes, no shard deviates more than 10% from the mean.
func TestDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("1M keys x 15 rings; skipped with -short")
	}
	const nKeys = 1_000_000
	keys := testKeys(nKeys)
	vnodeCounts := []int{1, 16, 64, 128, 256}
	shardCounts := []int{2, 3, 5}

	var out strings.Builder
	out.WriteString("| Virtual nodes | Shards | Max deviation from mean | Std deviation |\n")
	out.WriteString("| --- | --- | --- | --- |\n")
	for _, v := range vnodeCounts {
		for _, s := range shardCounts {
			maxDev, stdDev := spread(keys, v, s)
			fmt.Fprintf(&out, "| %d | %d | %.1f%% | %.1f%% |\n", v, s, maxDev, stdDev)
			if v == DefaultVirtualNodes && maxDev > 10 {
				t.Errorf("%d virtual nodes, %d shards: max deviation %.1f%% > 10%%", v, s, maxDev)
			}
		}
	}
	t.Logf("%d keys\n%s", nKeys, out.String())
}

// spread returns, as percentages of the mean keys per shard, the largest deviation of any
// shard and the standard deviation across shards.
func spread(keys [][]byte, vnodes, shards int) (maxDev, stdDev float64) {
	r := New(vnodes)
	for i := 1; i <= shards; i++ {
		r.Add(fmt.Sprintf("shard-%d", i))
	}
	counts := map[string]int{}
	for _, k := range keys {
		id, _ := r.Lookup(k)
		counts[id]++
	}
	mean := float64(len(keys)) / float64(shards)
	var sumSq float64
	for i := 1; i <= shards; i++ {
		d := float64(counts[fmt.Sprintf("shard-%d", i)]) - mean
		maxDev = math.Max(maxDev, math.Abs(d))
		sumSq += d * d
	}
	return 100 * maxDev / mean, 100 * math.Sqrt(sumSq/float64(shards)) / mean
}

// BenchmarkLookup measures the cost of one routing decision on the production-size ring.
func BenchmarkLookup(b *testing.B) {
	r := ringOf(DefaultVirtualNodes, "shard-1", "shard-2")
	keys := testKeys(1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Lookup(keys[i&1023])
	}
}
