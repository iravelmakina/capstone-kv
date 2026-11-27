# ADR-0036: Leader placement and planned leader transfer

- Status: **Accepted** — 2026-11-28 by Iryna Velmakina (author) in chat ("Accept and
  commit"), with the AI-review verdicts she confirmed ("Yes, apply them"). Drafted with
  Claude Code; revised after the AI design review (`docs/m5/ai-review.md`); the
  recommendation moved from option C to B + D there.
- Date: 2026-11-27
- Takes up ADR-0030's runner-up (option C), left for M5. ADR-0030's SLO (99%) stays.

## Context

ADR-0020 starts shard-1's leader on node a and shard-2's on node b, so a node's loss costs
at most one shard's writes. In M4 that didn't hold:

- **At start**, the first leader of a set is chosen once a majority of its replicas has
  registered and a timeout has passed (`assignFirstLeaderLocked`). When node a registered
  a minute or two late, both shards' first leaders went to node b: session 1 and session 3
  (`docs/m4/evidence/aws-deploy.txt`). Whichever node registers first gets a leader.
- **At promotion**, the coordinator takes the most up-to-date follower, whichever node it
  is on; after the lab restarted the instances in session 2, both leaders were on node a.
- **The cost** (drill D2): a node holding both leaders stopped → both shards' writes
  answered 503 for 14.7 s, ≈ 0.93% of an hour's requests under a client that doesn't back
  off. That is why ADR-0030 moved the SLO to 99%.
- **How replicas learn about leadership today:** only from the coordinator's heartbeat
  answers, every 5 s (ADR-0020). A follower re-points its pull after its heartbeat or its
  7 s pull deadline (ADR-0023). Anything that changes the leader on purpose must tell the
  replicas directly, or writes meet 409s and 504s for seconds (AI review W4).

In M5 a third replica set lives on its own three nodes (ADR-0035), so its leader can't
share a node with shard 1's or 2's; the issue is nodes a/b/c.

## Options

| | A. Leave it (99% SLO absorbs one such loss per hour) | **B. Placement at assignment: wait (bounded) for all expected replicas before the first leader, prefer a node without a leader at first assignment and at promotion** | C. B + automatic planned transfer by the coordinator | **D. B + planned transfer, run only when the ops agent proposes it and the author approves** |
| --- | --- | --- | --- | --- |
| Late node at start (seen twice) | both leaders on one node | **fixed when the node is within the wait (3 min)** | fixed | fixed |
| Promotion with equally caught-up followers | either node | **fixed** | fixed | **fixed** |
| Promotion where only the "wrong" node is caught up | stays | stays (safety first) | moved later, automatically | **moved when approved** |
| New safety-critical code | none | a tie-break and a wait | a transfer, triggered with nobody watching, also mid-benchmark | **the same transfer, triggered by a person** |
| Works with nobody watching | — | yes | yes | no; the store only runs while the author is there (ADR-0035) |

**Safety rule for every option:** only a follower with the highest `(epoch, LSN)` may be
promoted after a leader's loss (ADR-0020). A transfer is different: the old leader is
alive, so the target can first be brought fully up to date.

## Decision

**B + D.**

1. **Bounded wait at the first assignment:** a set's first leader is chosen once all its
   expected replicas are live, or after **3 minutes** with a majority, whichever comes
   first (today: a majority plus one 15 s timeout). The two late starts in M4 were "a
   minute or two".
2. **Tie-break at first assignment and at promotion:** among candidates with the highest
   `(epoch, LSN)`, prefer one on a node holding no other leader, then the preferred replica,
   then the lowest ID. A node is the address a replica heartbeats from.
3. **Planned transfer, fenced at the leader** (`POST /v1/admin/shards/{id}/leader {to}`,
   `admin` scope; executed by kvscale after an approved `transfer_leader` proposal, or by a
   person):
   1. the coordinator holds new writes to that shard (≤ 2 s budget for the whole transfer);
   2. it tells the leader L to **step down for epoch e**: L stops accepting writes (409),
      finishes or fails the commits in flight, and answers with its final committed LSN;
   3. it waits until the target T has acknowledged that LSN (its pull position, ADR-0023);
   4. it persists epoch e+1 with T as leader, and **pushes `(e+1, T)` synchronously to T,
      L and the third replica**; each acknowledges before the next step (followers re-point
      their pull to T at once);
   5. it releases the held writes to T.
   If L doesn't answer step 2, or T isn't caught up, within the budget, the transfer is
   abandoned: L is told to resume epoch e (nothing was persisted), and the writes are
   released to it. If a replica doesn't acknowledge step 4, its next heartbeat carries the
   new epoch as today.
4. **No automatic transfer.** The coordinator never moves a healthy leader on its own; the
   ops agent proposes it (an info-level `LeadersColocated` alert makes the situation an
   input, ADR-0034), and the author approves.
5. **Metric:** leaders per node (`kv_node_leaders`), on the dashboard and among the ops
   agent's named queries.

A transfer and a migration's cutover both hold writes, but they are different operations
(one changes epochs and roles, the other key ownership); they share only the hold. One
hold at a time: no transfer during a migration, and the reverse.

**SLO:** stays 99% per hour. A drill with one leader on the stopped node (the case
ADR-0030 estimated at ≈ 0.46%) would show whether 99.5% could come back; that is a later
ADR, not this one.

**Runner-up:** C. Reconsider if placement goes wrong often while nobody is there to
approve, which in this sandbox can't happen (the store runs only during lab sessions).

## Accepted downsides

- Start-up can take up to 3 minutes longer when a node is late.
- A promotion onto a node that already leads stays until someone approves a transfer.
- Each transfer holds one shard's writes for up to 2 s; p99 for that shard reads high for
  one rate window afterwards, so scaling waits that window (ADR-0032).
- A new step-down call and a synchronous epoch push in the coordinator's leadership path,
  which M3 tested without them.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A transfer loses an acknowledged write | High | The leader steps down first and reports its final committed LSN, so no write commits after the LSN the target must reach; a test injects a write in flight during step 2 and checks every acknowledged write afterwards |
| Two leaders during a transfer | High | L refuses writes from step 2; the coordinator persists and pushes the epoch; replicas obey it and answer 409 otherwise (ADR-0020) |
| L steps down and the coordinator dies before step 4 | Medium | L resumes on its next heartbeat when the persisted epoch is still e; writes meanwhile get 503 like during a promotion |
| Transfer during a migration's cutover | Medium | One hold at a time |

## Related

- ADR-0020 (placement, promotion), ADR-0023 (pull position as ack), ADR-0030 (the SLO and
  this runner-up), ADR-0033 (the other hold), ADR-0034 (agent's `transfer_leader`).
