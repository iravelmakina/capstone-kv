# ADR-0034: The limits of the ops agent's autonomy

- Status: **Accepted** — 2026-11-28 by Iryna Velmakina (author) in chat ("Accept and
  commit"), with the AI-review verdicts she confirmed ("Yes, apply them"). Drafted with
  Claude Code; revised after the AI design review (`docs/m5/ai-review.md`).
- Date: 2026-11-27

## Context

The brief requires an agent that consumes alerts and metrics and proposes an action
(rebalance, scale up, scale down) with its reasoning; the bonus closes the loop behind a
human approval gate. A control question asks what it proposes and why execution sits
behind approval.

What surrounds it:
- **The autoscaler already acts alone** (ADR-0032, ADR-0035): deterministic rules,
  unit-tested with `promtool`, within hard bounds. The agent is not the autoscaler; it
  covers what rules don't: a latency alert during a failure, leaders piled on one node
  (only the agent proposes moving a healthy leader, ADR-0036), a stalled migration, or a
  scaling decision the autoscaler can't take (switched off, or blocked by a guard).
- **Inputs can carry text from outside:** alert annotations are ours, but shard and
  replica names, executor messages and anything from logs could carry text someone chose
  (an item key in a log line is user-controlled). ADR-0027 already treats stored values as
  data; the same applies here.
- **Credentials:** scaling needs the author's lab credentials (instances can't change
  capacity, `docs/m1/labrole-dissection.md`), and Claude Code's auto-mode checker refuses
  deploys and deletes. The admin API needs a key with `admin` scope (ADR-0027). On the
  laptop, `~/.aws` holds the lab credentials while a session runs.
- **A wrong action is expensive:** a scale-in during a failure or a leader moved to a
  lagging replica costs availability; a scale-out costs money and a migration.

## Options

| | L0. Advisory only | **L1. Proposes; each proposal runs after approval** | L2. Acts alone within bounds, asks for the rest | L3. Fully autonomous |
| --- | --- | --- | --- | --- |
| Brief | required part | **required part + bonus** | bonus, beyond it | — |
| Who executes | a person, by hand | the executor, after a person approves that proposal | the agent (needs credentials) | the agent |
| A misread metric or an injected instruction causes | nothing | nothing, unless a person approves it | an action within bounds | anything |
| Agent holds credentials | no | **no** | yes | yes |
| Measured / testable | yes | yes: the gate and checks are code; the judgement by an evaluation set | the agent's judgement isn't unit-testable | no |

## Decision

**L1.** The agent proposes; nothing it says runs without the author approving that
specific proposal.

1. **Actions it may propose** (a closed list, typed):
   - `transfer_leader{shard, to}`: the "rebalance" of leadership, and the only way a
     healthy leader moves (ADR-0036);
   - `abort_migration`: the "rebalance" of data when a migration is stalled, allowed only
     before any cutover (ADR-0033; migrations resume on their own after a restart, so
     there is no "resume");
   - `scale_up`, `scale_down`: **only when the autoscaler is switched off or blocked**, and
     the proposal names the guard that blocks it, so the agent never duplicates or
     contradicts the rule;
   - `no_action`.
   Each carries the reasoning, the values and alerts it used with their timestamps, the
   expected effect, and the risk of acting and of not acting.
2. **Read-only inputs, chosen by us:** firing alerts (including the info-level
   `LeadersColocated` and `MigrationStalled` alerts added for it), a fixed list of named
   queries (no free-form PromQL), cluster state, the executor's recent decisions, the
   running release. No item reads, no log search, no shell.
3. **No credentials, enforced by the OS, not only by the code:** `kvops` runs in a
   container that mounts neither `~/.aws` nor the admin key. Its only secret is an API key
   with a new **`status`** scope: the admin API's read-only cluster state, and no item
   routes (`read` would let a compromised agent read items).
4. **Checked before shown:** a proposal outside the list, outside the scaling bounds, or
   naming a shard, replica or node that doesn't exist is shown as rejected and can't be
   approved.
5. **The gate is a separate command** (`kvops approve <id>`), run by the author outside the
   container, which prints the proposal and asks. The agent writes proposals to a
   directory it may only add to; approvals are written by the gate to a place the
   container can't reach. A proposal expires 10 minutes after it was made.
6. **The executor re-checks** the proposal's preconditions and every scaling guard
   (ADR-0032) against the current state before acting, and refuses if any fails. Approval
   doesn't bypass a guard.
7. **Everything is recorded:** inputs, tool results, proposal, check, approval, outcome;
   the proposal goes to the Telegram chat through Alertmanager.
8. **Testing:** the deterministic parts (schema, checks, bounds, expiry, gate, guards) get
   unit tests; the agent's judgement gets an **evaluation set** (recorded situations with
   the expected proposal kind) and a pass rate, run on request because it calls the model.

Why execution sits behind approval:
- The agent's judgement is a model's output: it can be wrong in ways no unit test catches,
  and text in its inputs can try to steer it. The gate makes a wrong proposal cost a
  person's attention, not availability.
- The actions it covers are the rare, situational ones; the frequent, mechanical one
  (scaling on p99) is already automatic and tested. A person approving a handful of
  proposals per session is cheap.
- Scaling needs the author's lab credentials anyway; giving them to an agent process would
  mean storing them somewhere, which the project forbids.

**Runner-up:** L0, if the bonus is dropped. Reconsider L2 for `transfer_leader` only once
the evaluation set and several sessions show the agent's proposals match the author's
choices.

## Accepted downsides

- Nothing happens while the author is away: a proposal expires unapproved. The autoscaler
  and the alerts still work.
- The agent sees only what the fixed queries show; an unusual incident needs a person with
  Grafana.
- A person can still approve a bad proposal; the guards limit what that can do.
- The evaluation set costs model calls each time it runs.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Prompt injection through alert labels, names or messages | Medium | Inputs passed as structured data; closed action list and bounds checked in code; approval; no item or log access |
| The agent proposes scaling into a failure | Medium | Guards refuse it; the evaluation set covers "latency with a node down" |
| Approval fatigue: approving without reading | Low | Few proposals; the command shows reasoning and numbers; the guards are re-checked anyway |
| The agent's model or API is unavailable | Low | The agent is optional at run time; the autoscaler and alerts don't depend on it |

## Related

- ADR-0027 (keys, scopes, injection), ADR-0032 (thresholds, guards), ADR-0033 (migration),
  ADR-0035 (executor), ADR-0036 (leader transfer). Runtime and model: `design.md`.
