# Ring distribution (ADR-0011)

How evenly the consistent hashing ring spreads keys, measured to choose the number of
virtual nodes per shard. Test: `internal/ring/distribution_test.go`, run with
`go test ./internal/ring/ -run TestDistribution -v` (2026-11-24, Go 1.24, xxHash64).

**Setup:** 1,000,000 routing keys in table `usertable` with sequential partition keys
`user0` … `user999999`: very similar strings, a harder case for a weak hash than the
benchmark's own keys (with `insertorder=hashed`, YCSB names records `user` + a hashed
number). Shard IDs are the ones the deployment uses: `shard-1` … `shard-5`. The result
holds for these IDs; M5, with other IDs, should check the worst case over many ID sets
(AI review §4.11).
"Deviation" is a shard's key count minus the mean, as a percentage of the mean.

| Virtual nodes | Shards | Max deviation from mean | Std deviation |
| --- | --- | --- | --- |
| 1 | 2 | 31.6% | 31.6% |
| 1 | 3 | 97.5% | 73.8% |
| 1 | 5 | 166.4% | 94.7% |
| 16 | 2 | 12.1% | 12.1% |
| 16 | 3 | 28.1% | 19.9% |
| 16 | 5 | 33.0% | 21.1% |
| 64 | 2 | 0.5% | 0.5% |
| 64 | 3 | 27.0% | 20.7% |
| 64 | 5 | 17.1% | 11.3% |
| **128** | **2** | **2.5%** | **2.5%** |
| **128** | **3** | **8.7%** | **6.9%** |
| **128** | **5** | **7.5%** | **5.4%** |
| 256 | 2 | 6.0% | 6.0% |
| 256 | 3 | 2.4% | 1.9% |
| 256 | 5 | 6.7% | 4.8% |

## Result

**128 virtual nodes stays.** ADR-0011's rule: the smallest count where no shard deviates
more than 10% from the mean, for 2, 3 and 5 shards.
- **64 fails:** 27% with 3 shards (one shard gets about a quarter more keys than the mean).
- **128 passes:** worst case 8.7%.
- **256** is better (worst 6.7%) but doubles the ring for a gain we don't need yet.

## What the numbers show

- **1 point per shard is unusable:** with 5 shards one shard holds 2.7× the mean. Arcs
  between single points are random lengths.
- **More points → smaller spread, but not smoothly:** 64 gives 0.5% with 2 shards and 27%
  with 3. The spread is random around ~1/√(points); each placement is one draw. That's
  why the rule checks several shard counts, not one.
- **These are the exact placements M2–M5 will get:** positions depend only on the shard
  ID (`hash("shard-N#i")`), and the deployment uses these IDs.

## Other measurements (same run)

| | Result |
| --- | --- |
| Adding a 3rd shard to 2 (100,000 keys) | about 1/3 of the keys move, all to the new shard; none between the old two (`TestJoinMovesAboutOneThirdOnlyToNewShard`) |
| Shard restarts with the same ID | no key moves (`TestRestartWithSameIDMovesNothing`) |
| One lookup, 2 shards × 128 points | 25 ns (`BenchmarkLookup`, Apple M-series laptop) |
