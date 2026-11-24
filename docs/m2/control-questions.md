# M2 control questions — short answers

> **Drafted with Claude Code** as defense prep. The author answers these orally at the
> defense; this file is notes, not the author's own text. Each answer points to where this
> project shows it. Questions 9–12 are the ones the
> lecturer asked in the 20.11 defenses beyond the brief's list.

## 1. Types of scalability, and when to use which

- **Vertical (scale up):** a bigger machine. Simple, no code changes, but it has a ceiling
  and one machine stays one failure. Here the ceiling is hard: the lab denies anything above
  t3.medium (M1 evidence 39–40).
- **Horizontal (scale out):** more machines sharing the work. No ceiling in principle, but
  the system has to split work and data: that's what sharding is. Our M2: 2 shards; M5
  adds more.
- Rule of thumb: scale stateless parts by adding identical copies; scale stateful parts
  (the shards) by partitioning data across them. Our coordinator is in between: no items,
  but it holds the table registry and the membership (question 7).

## 2. Types of sharding, with use cases

| Type | How a key finds its shard | Good for | Weak at |
| --- | --- | --- | --- |
| **Hash** | hash(key) → shard | even spread, point lookups (KV stores) | range queries scan every shard |
| **Range** | key ranges per shard | range scans, time series | hot ranges (e.g. "today") |
| **Directory** | a lookup table key → shard | moving single keys, uneven data | one more component to keep up |
| **Geo / entity** | by region or tenant | data residency, tenant isolation | uneven tenants |

Ours: **hash sharding with consistent hashing** (ADR-0011), because the workload is point
reads and writes by key. Also: **horizontal partitioning** (split rows, which is sharding)
vs **vertical partitioning** (split columns, e.g. images in one store, text in another).

## 3. The consistent hashing algorithm

1. Picture all 64-bit hash values as a circle.
2. Each shard is placed on it at many points: `xxhash64("shard-1#0")`, `…#1`, … 128 times
   (virtual nodes, ADR-0011).
3. A key is hashed onto the same circle (`table + 0x00 + partition key`, ADR-0012) and
   belongs to the **first shard point clockwise**.
4. Lookup = binary search over the sorted points: 25 ns here (`docs/m2/ring-distribution.md`).

Why: with plain `hash(key) % N`, changing N moves almost every key. On the ring, adding a
shard moves only the keys that now fall before its points: ~1/(N+1) of them. Measured: a
third shard took ~1/3 of the keys and no key moved between the two old shards.

## 4. How does a HashMap work?

An array of buckets. `hash(key) % capacity` picks the bucket; keys that land in the same
bucket (collisions) are kept in a list or the next free slot. Lookups are O(1) on average.
When it gets too full (load factor), it **grows and rehashes**: almost every key moves to a
new bucket. That's fine in one process, but the same idea across machines would mean moving
almost all data when a shard is added, hence consistent hashing instead. Each of our shards
keeps its keys in a Go map (a hash map) in memory (`internal/store`).

## 5. Why do we need partition and sort keys?

- **Partition key:** decides **where** an item lives (which shard). Items with the same
  partition key are always together on one shard.
- **Sort key:** decides **order within** the partition and makes the full key unique, so a
  partition can hold many items ("all orders of customer 7, by order id").
- Our API: `/v1/tables/{t}/items/{pk}` or `/{pk}/{sk}`; only `table + pk` is hashed
  (ADR-0012), so a future "all items for pk, sorted by sk" query touches one shard.

## 6. Linearizability and serializability

- **Linearizability:** about **single objects** and real time. Every operation appears to
  happen at one instant between its start and its end; once a write is acknowledged, every
  later read sees it.
- **Serializability:** about **transactions over many objects**. The result is as if the
  transactions ran one after another, in *some* order, not necessarily real-time order.
- **Strict serializability** = both.
- Our M2: each key has **one copy on one shard**, behind a lock, written to disk before the
  reply, so single-key operations are linearizable. There are no multi-key transactions,
  so serializability doesn't arise. M3 adds replicas, and keeping reads fresh becomes the
  R + W > N question.

## 7. Is your coordinator a bottleneck? How would you scale it?

**Yes, the next one.** In the benchmark it handled every request twice (client side and
shard side), averaged 39% CPU with peaks of 69%, and used up its CPU credits; with
`standard` credits it would have been throttled (`docs/m2/benchmark.md`). It's also a
single point of failure.

How to scale it: it holds no items, but it isn't stateless either: the table registry and
the shard membership live in its memory, and shards heartbeat to its one address. So first
move both to a shared place, then
- run **several coordinators behind a load balancer**; shards register with the shared
  registry (or with every coordinator);
- or **client-side routing**: give clients the ring and let them call shards directly
  (what DynamoDB's request routers or Cassandra drivers do), removing a network hop.

## 8. Which YCSB workload did you choose, and why is it representative?

**A** (50% read / 50% update) and **B** (95% / 5%), both zipfian (a few hot keys get most
requests, like real users). A stresses the write path (session stores); B is the common
key-value case (profiles, caches). C is a subset of B; E needs range scans, which M2 doesn't
have. Results: A 1,804 ops/s, B 6,891 ops/s (`docs/m2/benchmark.md`).

## 9. Five shards, one fails: what's the problem, how do virtual nodes help?

- With **one point per shard**, all keys of the failed shard fall to **its one clockwise
  neighbour**, which suddenly has double the load.
- With **virtual nodes**, the failed shard's 128 points are scattered round the ring, so its
  keys spread over **all** the remaining shards, each taking a small share. The same is
  true when a shard is added: it takes a little from everyone.
- Our M2 detail: a silent shard **stays on the ring** and its keys answer 503 (ADR-0013),
  because without replicas the neighbours don't have its data. Remapping only makes sense
  once there are copies (M3) or migration (M5).

## 10. What matters most when choosing a partition key?

**The access pattern** first: the queries you run must hit one shard. If a common query has
to scan or join across shards, an even spread doesn't help. Then: high cardinality (many
distinct values), even load (no hot keys), and the value never changes after the item is
written. YCSB keys (`user<number>`) are high-cardinality and accessed one at a time.

## 11. Load balancing and service discovery

- **Load balancing:** spreading requests over several instances of the same service (round
  robin, least connections), with health checks so dead instances get no traffic. Layer 4
  (TCP, e.g. an NLB) or layer 7 (HTTP, e.g. an ALB, which can route by path).
- **Service discovery:** how a caller finds the current instances of a service: a registry
  (Consul, Cloud Map), DNS, or self-registration.
- Ours: **self-registration with heartbeats**: each shard tells the coordinator "I'm shard-1
  at 10.20.78.207:8080" at start and every 5 s; 3 missed heartbeats = unavailable. No load
  balancer in M2 (ADR-0014); one would sit in front of several coordinators.

## 12. Why is p99 so much higher than the average, or than p95?

The average hides the tail. In workload A an update averages 15 ms but p99 is 33 ms: a
request that arrives behind a queue of writes on its shard waits for all of them, because
the shard holds one lock across each `fsync`. Rare stalls (EBS hiccups, Go garbage collection, a busy coordinator) push
p99.99 and max to 100–250 ms. Users notice the tail, which is why M5 scales on p99.
