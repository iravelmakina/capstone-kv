# ADR-0030: The availability SLO after the AWS drills

- Status: **Accepted** — 2026-11-27 by Iryna Velmakina (author) in chat ("Accept it"),
  options B and a. Drafted with Claude Code at her request ("Yes, draft it") from the M4
  lab drills.
- Date: 2026-11-27
- Supersedes in part ADR-0026: the availability value and the arithmetic behind it ("fits
  about three leader changes per hour"), and its worry about reads during a node stop.
  Read and write p99 (15 and 60 ms) are unchanged.

## Context

ADR-0026 chose 99.5% per hour, request-based, because one leader change was expected to
cost ≈ 20 s of 503 on **one** shard's writes, ≈ 0.14% of an hour's requests, so about
three would fit. Task 8.2 asked for drills to check that. What they showed
(`docs/m4/evidence/aws-deploy.txt`):

| Drill | What happened | Against ADR-0026 |
| --- | --- | --- |
| D2: the node with shard-1's leader stopped, workload A loading | That node held **both** leaders (1b and 2b). Both shards answered writes with 503 for 14.7 s, then promoted. 50,322 5xx out of 1.1 M requests in the run (4.6%) | far above 0.14% |
| D1': a node with only followers stopped, workload A running (~960 req/s) | 0 5xx in 287 s; read p99 7.9 ms; no read over 1 s | better than feared: no 3 s waits |
| S5: right after deploy, almost no traffic | 4 probe 503s while the cluster formed fired ErrorBudgetFastBurn and ErrorBudgetSpent | a page for nothing |

Why D2 cost so much:

1. **Two leaders on one node.** Shard leaders start on different nodes (ADR-0020), but
   a promotion takes the most up-to-date follower, whichever node it is on. It isn't
   rare: session 1 started with both leaders on node B (node A registered late), and
   session 2, after the lab restarted the instances, had both on node A.
2. **The benchmark client doesn't back off.** A 503 comes back in ~5 ms against ~23 ms
   for a commit. go-ycsb's 16 threads went straight to the next operation, so in 14.7 s
   they used up 50,320 inserts, half the load phase, all failing. The SLI counts requests,
   so a client that fails fast and keeps going makes the same outage count several times
   more.

At workload A's ~1,500 requests/s, an hour is ~5.4 M requests, and D2's 50 k failures are
≈ 0.93% of it. With one leader on the node it would be about half that (an estimate: not
measured). This was the load phase (inserts only). In the read/update phase, reads keep
working, so a node loss would cost less there; that wasn't measured.

S5 is the same SLI at the other end. With no traffic, the probe (one request per 5 s) is
almost all there is, and four failures out of a few dozen are over 5%.

## Decision 1: the availability value

| | A. Keep 99.5%, and say that a leader-node loss breaks it | **B. 99% per hour** | C. Keep 99.5%; the coordinator keeps leaders on different nodes |
| --- | --- | --- | --- |
| D2 (both leaders, 0.93% of an hour) | breaks it | **fits, once an hour** | can't happen: one leader per node |
| One leader on the node (≈ 0.46%, estimated) | fits once, with nothing left | fits about twice | fits once |
| Same method as ADR-0026 (the SLO fits the failure we expect) | no | **yes** | yes |
| Work | runbook and ADR text | thresholds in rules, dashboard, spec, runbook | Go code in the coordinator (promotion and a rebalance at start), tests, a drill |
| Hides a real problem? | no | a little: 36 s of a full outage per hour pass | no |

**Option B.**

- **Availability ≥ 99%** of public requests answered below 500 and of probes, per hour of
  operation. The SLI itself (ADR-0025) is unchanged.
- Fast burn keeps ADR-0026's rule that the hour's budget would be gone in about 6 minutes:
  5xx above **10%** over both 5 minutes and 1 minute (was 5%). "Budget spent": over **1%**
  in the hour (was 0.5%). In D2, about 85% of requests failed during the 14.7 s (3,417
  failing against 579 succeeding per second), and over the 5 minutes after the stop it was
  about 13% (an estimate from go-ycsb's counts). So D2 still pages, but with less margin
  than at 5%. A leader-node loss during the read/update phase, where reads keep working,
  might stay under 10%. NodeDown (critical) still fires then. ShardWithoutLeader fires only
  if the promotion takes more than 30 s.
- The numbers come from what we measured, by ADR-0026's own method. The worst case seen
  in AWS was a node with both leaders under a client that doesn't back off, at 0.93% of an
  hour. It fits once per hour, so the SLO tolerates one unlucky node loss and is breached
  by a second, or by a shard that is down for longer than ≈ 36 s.

**Runner-up: C**, in M5. Placement belongs with M5's rebalancing and autoscaling, and it
needs changes in the coordinator's promotion, which M3 tested. With C, 99.5% could come
back.

## Decision 2: no burn alerts at almost no traffic

| | **a. Burn alerts only at ≥ 1 request/s over 5 min** | b. Leave them as they are | c. Count probes apart from requests |
| --- | --- | --- | --- |
| S5 (4 probe 503s at idle) | no page | pages | probes only: still four failures out of a few dozen |
| A dead store at idle | CoordinatorDown (probe), ShardWithoutLeader, NodeDown still fire | pages | pages |
| Same as the latency alerts | yes (they already require ≥ 1 req/s) | — | — |

**Option a.** `ErrorBudgetFastBurn` and `ErrorBudgetSpent` fire only while the
store gets at least 1 request/s over 5 minutes. When there is no traffic, the per-cause
alerts cover a broken store, and they don't depend on how many requests there are.

## What doesn't change

- Read p99 ≤ 15 ms and write p99 ≤ 60 ms per shard over 5 minutes. In D2, shard-2's read
  p99 was 19.8 ms over the 2 minutes around the promotion. That's below the 5-minute
  window the alert uses, so it is expected and doesn't page. In D1' and in the benchmark it
  was 7.7–7.9 ms.
- ADR-0026's note about reads waiting out the 3 s client timeout when a node stops: D1'
  didn't show it (read max 227 ms, no 5xx), so that worry is dropped.

## If accepted

- Spec (`specs/observability/spec.md`), `design.md` and `proposal.md`: 99.5% → 99%,
  fast burn 5% → 10%, budget 0.5% → 1%, the traffic guard. This goes in a spec commit,
  before any code.
- Code (a separate commit): `monitoring/rules.yml` (thresholds, guard),
  `monitoring/rules_test.yml` (a test that S5 doesn't page; D2 still does), the dashboard's
  SLO lines (0.995 → 0.99), and the runbook (its "0.14%, three per hour" line).
- ADR-0026 gets a "Superseded in part by ADR-0030" line; its text stays.
- Task 8.2 is ticked.

## Accepted downsides

- An hour can now hold 36 s of a full outage without breaching the SLO.
- The SLO is still tuned to a benchmark client that doesn't back off: real clients with
  backoff would fail fewer requests in the same outage, so for them it is looser still.
- A store that is idle and partly broken (one shard down, nobody asking) doesn't page
  through the budget alerts, only through the cause alerts.

## Related

- ADR-0026 (superseded in part), ADR-0025 (SLIs), ADR-0029 (alerts), ADR-0020 (leader
  placement and promotion), `docs/m4/evidence/aws-deploy.txt` (D1', D2, S5).
