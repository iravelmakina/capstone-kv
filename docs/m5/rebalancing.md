# M5 rebalancing: shard sizes before and after

> Written by Claude Code on 2026-11-28 from the runs below, which it made itself on the
> local Docker cluster (`make local-join`, `make local-leave`). The AWS section (task 7.2)
> is added in the first lab session. Spec `shard-rebalancing` "Progress, alerts and
> evidence"; ADR-0033 (algorithm), ADR-0011 (ring, 128 virtual nodes, 10% target).

## Local: shard-3 joins, then leaves

**Setup:** the local Docker Compose cluster (coordinator, shard-1 and shard-2 with three
replicas each, W = 2 / R = 2) on the author's laptop, release built from the working tree
(`version: dev`), migration rate 2,000 records/s (the default). Table `users` held 48,576
items: 20,000 seeded by `make local-seed N=20000` for this run, the rest left in the local
volumes by earlier M4 runs. During each migration `scripts/local-migrate.sh` ran a
verifier: one client writing new keys through the coordinator back to back, then reading
every acknowledged key back.

Sizes are each replica set's leader's `/health` (`keys`, `logBytes`); the same numbers are
`kv_store_keys` and `kv_log_bytes` on the dashboard.

### Join (`make local-join`, migration `add-shard-3-1795881625`)

| Set | Keys before | Keys after | Log before | Log after |
| --- | ---: | ---: | ---: | ---: |
| shard-1 | 24,727 | 15,278 | 2.79 MB | 3.00 MB |
| shard-2 | 23,849 | 16,727 | 2.70 MB | 2.89 MB |
| shard-3 | 0 (pending) | 18,136 | 0 | 1.21 MB |
| **Total** | **48,576** | **50,141** | | |

- **Took 10 s** from `POST /v1/admin/migrations` to phase `done` (snapshot, tail and a
  fenced cutover for each of the two sources, purge, unfreeze).
- **Verifier:** 1,564 writes during the migration, 1,564 acknowledged, 0 answered 5xx, 0
  acknowledged writes missing or with another value afterwards.
- **Counts add up:** 48,576 + 1,564 verifier keys = 50,140. The one extra key is the
  verifier's last write: the script stops the writer loop while its last `curl` is in
  flight, so that write reaches the store but isn't in the verifier's log.
- **Spread:** mean 16,714 keys per set; shard-1 −8.6%, shard-2 +0.1%, shard-3 +8.5%: within
  ADR-0011's 10%. Before the join the two sets were within 1.8% of each other; each now
  holds about a third instead of a half.
- **Net change:** shard-1 and shard-2 ended with 9,449 and 7,122 fewer keys (net: they also
  kept the verifier's new keys that hash to them), shard-3 with 18,136. These counts can't
  prove that only moving keys moved; `TestCopyStateThenTail` and the runner tests check that
  key by key.
- **Logs grow on the sources** (2.79 → 3.00 MB): purge records drop keys from memory, not
  from the log, which is never compacted (ADR-0033, accepted downside).

### Leave (`make local-leave`, migration `remove-shard-3-1795881665`)

| Set | Keys before | Keys after | Log before | Log after |
| --- | ---: | ---: | ---: | ---: |
| shard-1 | 15,278 | 26,307 | 3.00 MB | 3.75 MB |
| shard-2 | 16,727 | 25,234 | 2.89 MB | 3.46 MB |
| shard-3 | 18,136 | 0 (retired) | 1.21 MB | 1.55 MB |
| **Total** | **50,141** | **51,541** | | |

- **Took 10 s**; shard-3 then stopped and forgotten by the coordinator (204). Ring back to
  `[shard-1, shard-2]`.
- **Verifier:** 1,399 writes, 1,399 acknowledged, 0 5xx, 0 missing. 50,141 + 1,399 = 51,540;
  the extra one is again the verifier's last in-flight write.
- **Spread after:** shard-1 +2.1%, shard-2 −2.1% of the mean.

### What this shows and what it doesn't

- It shows the whole path working on real processes and disks: pending registration, the
  admin API, copy, committed tail, freeze and cutover per source, purge, unfreeze, the
  ring change, retirement, with writes flowing and none lost.
- The load was light: one serial writer (≈ 150 writes/s). The requests-per-second figures
  the script printed from the local Prometheus are 1-minute rates and lag the change, so
  they are left out here. Load during migration, a hold's effect on p99, and the copy's
  effect on the source's write p99 are measured in AWS (tasks 7.2, 8.1).
- Writes injected into the cutover's windows, a source leader dying with an uncommitted
  record, abort and restart are covered by the Go tests (`internal/coordinator/runner_test.go`,
  `internal/shard/migrate_test.go`), including a 100,000-key add and remove under four
  writers: 30,889 / 33,403 / 36,408 keys after the add.

## AWS

To be added in lab session 1 (task 8.2): the same join and leave under load, with the
dashboard's keys, log bytes and request rate per shard, the 5xx count, the longest write
hold, and the verifier's result.
