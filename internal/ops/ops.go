// Package ops is the ops agent's deterministic part (ADR-0034, spec ops-agent): the typed
// proposal and its checks, the read-only tools the agent may call, where proposals and
// approvals are kept, and the approval gate. The model loop lives in cmd/kvops (its own
// module, with the Anthropic SDK); nothing here calls a model, so all of it is unit-tested.
//
// Limits enforced here, not by the model's good behaviour:
//   - the agent's tools only read: firing alerts, a fixed list of named metric queries,
//     the cluster state (with a key that has only the status scope), the executor's recent
//     decisions, the running release; no item, log or free-form query tool;
//   - a proposal is one of five kinds with typed parameters, checked against the current
//     state before anyone sees it as approvable;
//   - approvals are written by a separate command (Approve), to a directory the agent's
//     container doesn't mount; a proposal expires 10 minutes after it was made.
package ops

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
)

// Kinds of proposal (ADR-0034).
const (
	TransferLeader = "transfer_leader" // move a shard's leadership to a follower on a leaderless node
	AbortMigration = "abort_migration" // a stalled migration, before any cutover
	ScaleUp        = "scale_up"        // only when the autoscaler is off or blocked
	ScaleDown      = "scale_down"      // likewise
	NoAction       = "no_action"
)

// Kinds lists them, for the schema and the checks.
var Kinds = []string{TransferLeader, AbortMigration, ScaleUp, ScaleDown, NoAction}

// Expiry is how long a proposal can be approved (ADR-0034).
const Expiry = 10 * time.Minute

// Evidence is one value or alert a proposal relied on.
type Evidence struct {
	Source string `json:"source"` // a tool and query name, e.g. "metric:write_p99"
	Value  string `json:"value"`
	At     string `json:"at"` // when it was read (RFC 3339)
}

// Proposal is what one agent run ends with.
type Proposal struct {
	ID        string     `json:"id"`
	Created   time.Time  `json:"created"`
	Kind      string     `json:"kind"`
	Shard     string     `json:"shard,omitempty"`   // transfer_leader
	To        string     `json:"to,omitempty"`      // transfer_leader: the follower that will lead
	Blocker   string     `json:"blocker,omitempty"` // scale_up/scale_down: why the autoscaler can't act ("off" or the guard)
	Reasoning string     `json:"reasoning"`
	Evidence  []Evidence `json:"evidence"`
	Effect    string     `json:"expectedEffect"`
	RiskAct   string     `json:"riskOfActing"`
	RiskWait  string     `json:"riskOfNotActing"`
	// Set by Check, not by the model:
	Rejected string `json:"rejected,omitempty"` // why it can't be approved
}

// Context is what Check needs about the world, read when the proposal is checked.
type Context struct {
	Cluster        coordinator.ClusterState
	AutoscalerOn   bool   // kvscale runs with autoscaling on
	AutoscalerHold string // the guard currently blocking it ("" if none)
}

// Check fills Rejected when the proposal is outside what may be approved: an unknown kind,
// missing or unknown parameters, an abort after a cutover, scaling outside the bounds or
// while the autoscaler would do it itself. It never changes anything else.
func Check(p *Proposal, c Context) {
	p.Rejected = check(p, c)
}

func check(p *Proposal, c Context) string {
	if !slices.Contains(Kinds, p.Kind) {
		return fmt.Sprintf("unknown kind %q", p.Kind)
	}
	if strings.TrimSpace(p.Reasoning) == "" {
		return "no reasoning"
	}
	sets := map[string]coordinator.ShardStatus{}
	for _, s := range c.Cluster.Sets {
		sets[s.ID] = s
	}
	ring := len(c.Cluster.Ring)
	switch p.Kind {
	case TransferLeader:
		s, ok := sets[p.Shard]
		if !ok {
			return "no shard " + p.Shard
		}
		found := false
		for _, r := range s.Replicas {
			if r.ID == p.To {
				found = true
				if !r.Available {
					return p.To + " is not live"
				}
			}
		}
		if !found {
			return "no replica " + p.To + " in " + p.Shard
		}
		if s.Leader == p.To {
			return p.To + " already leads " + p.Shard
		}
		if c.Cluster.Migration.Running() {
			return "a migration is running: no transfer during it (one hold at a time)"
		}
	case AbortMigration:
		mg := c.Cluster.Migration
		if !mg.Running() {
			return "no migration is running"
		}
		for src, sp := range mg.Sources {
			if sp.CutOver {
				return "source " + src + " has cut over: the migration can only finish"
			}
		}
	case ScaleUp, ScaleDown:
		if c.AutoscalerOn && c.AutoscalerHold == "" {
			return "the autoscaler is on and not blocked: it decides scaling itself"
		}
		if p.Kind == ScaleUp && ring >= 3 {
			return "upper bound: 3 replica sets"
		}
		if p.Kind == ScaleDown && ring <= 2 {
			return "lower bound: 2 replica sets"
		}
		if strings.TrimSpace(p.Blocker) == "" {
			return "a scaling proposal must name what blocks the autoscaler"
		}
	}
	return ""
}

// Expired reports whether the proposal can no longer be approved.
func (p Proposal) Expired(now time.Time) bool { return now.Sub(p.Created) > Expiry }

// Summary is the proposal as a person reads it before approving.
func (p Proposal) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proposal %s (%s ago): %s", p.ID, time.Since(p.Created).Round(time.Second), p.Kind)
	if p.Shard != "" {
		fmt.Fprintf(&b, " %s -> %s", p.Shard, p.To)
	}
	if p.Blocker != "" {
		fmt.Fprintf(&b, " (autoscaler: %s)", p.Blocker)
	}
	fmt.Fprintf(&b, "\n\nReasoning: %s\nExpected effect: %s\nRisk of acting: %s\nRisk of not acting: %s\nEvidence:\n",
		p.Reasoning, p.Effect, p.RiskAct, p.RiskWait)
	for _, e := range p.Evidence {
		fmt.Fprintf(&b, "  - %s = %s (at %s)\n", e.Source, e.Value, e.At)
	}
	if p.Rejected != "" {
		fmt.Fprintf(&b, "\nREJECTED by the checks: %s (it can't be approved)\n", p.Rejected)
	}
	return b.String()
}
