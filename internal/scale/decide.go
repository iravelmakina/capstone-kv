// Package scale is the autoscaling executor's logic (ADR-0032, ADR-0035; spec autoscaling):
// what to do now, decided from the current state only, so a restarted executor carries on
// where the last one stopped. cmd/kvscale runs it against Prometheus, the coordinator's
// admin API, Alertmanager and the extra replica set's capacity (AWS Auto Scaling groups or
// the local Compose services).
package scale

import (
	"fmt"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
)

// ExtraSet is the replica set the executor adds and removes (ADR-0035: 2-3 sets).
const ExtraSet = "shard-3"

// Action is what the executor does this tick.
type Action string

const (
	None         Action = "none"
	LaunchSet    Action = "launch"        // scale-out step 1: start the extra set's instances
	StartJoin    Action = "join"          // scale-out step 2: migrate it onto the ring
	StartLeave   Action = "leave"         // scale-in step 1: migrate it off the ring
	StopSet      Action = "stop"          // scale-in step 2: stop its instances
	ForgetSet    Action = "forget"        // scale-in step 3: the coordinator drops the retired set
	CancelLaunch Action = "cancel-launch" // instances that never registered in time
)

// Config holds the thresholds of ADR-0032 that live in the executor (the latency ones live
// in the Prometheus rules).
type Config struct {
	Cooldown      time.Duration // after a migration finishes; 10 min
	HoldWindow    time.Duration // no decision this long after a write hold; 5 min (the rate window)
	LaunchTimeout time.Duration // instances that don't register and serve within this are stopped; 10 min
}

func (c Config) withDefaults() Config {
	if c.Cooldown <= 0 {
		c.Cooldown = 10 * time.Minute
	}
	if c.HoldWindow <= 0 {
		c.HoldWindow = 5 * time.Minute
	}
	if c.LaunchTimeout <= 0 {
		c.LaunchTimeout = 10 * time.Minute
	}
	return c
}

// Inputs is everything a decision looks at, read fresh each tick.
type Inputs struct {
	Cluster          coordinator.ClusterState
	ScaleOutDue      []string // shards with a firing ScaleOutDue (any trigger)
	ScaleInOK        bool     // ScaleInLatencyOk is firing
	CoordinatorBound []string // shards with a firing CoordinatorBound
	Rate             float64  // requests/s over the last 10 minutes, whole store
	HoldsRecently    bool     // a write hold (transfer or cutover) ended within HoldWindow
	Instances        int      // the extra set's running instances (0-3)
	LaunchedAt       time.Time
}

// Decision is the action and why; Blocked names the guard that stopped an action that was
// otherwise due.
type Decision struct {
	Action  Action
	Reason  string
	Blocked string
	Rate    float64 // the request rate a join records (ADR-0032's scale-in rule)
}

// Decide works out the next step. Order matters: a running migration is left alone; the
// extra set's own lifecycle is advanced before any new decision; then scale-in (3 sets) or
// scale-out (2 sets), each behind its guards.
func Decide(now time.Time, in Inputs, cfg Config) Decision {
	cfg = cfg.withDefaults()
	mg := in.Cluster.Migration
	if mg.Running() {
		return Decision{Action: None, Reason: fmt.Sprintf("migration %s is %s", mg.ID, mg.Phase)}
	}
	var extra *coordinator.ShardStatus
	for i := range in.Cluster.Sets {
		if in.Cluster.Sets[i].ID == ExtraSet {
			extra = &in.Cluster.Sets[i]
		}
	}

	switch {
	case extra != nil && extra.State == coordinator.StateRetired:
		if in.Instances > 0 {
			return Decision{Action: StopSet, Reason: ExtraSet + " is retired: stop its instances"}
		}
		return Decision{Action: ForgetSet, Reason: ExtraSet + " is retired and stopped: the coordinator forgets it"}
	case extra != nil && extra.State == coordinator.StatePending:
		switch {
		case in.Instances > 0 && extra.Serving:
			return Decision{Action: StartJoin, Reason: ExtraSet + " registered and serves: migrate its keys in", Rate: in.Rate}
		case in.Instances > 0 && !in.LaunchedAt.IsZero() && now.Sub(in.LaunchedAt) > cfg.LaunchTimeout:
			return Decision{Action: CancelLaunch, Reason: fmt.Sprintf("%s registered but doesn't serve after %s", ExtraSet, cfg.LaunchTimeout)}
		case in.Instances > 0:
			return Decision{Action: None, Reason: ExtraSet + " registered; waiting for a confirmed leader and a quorum"}
		default:
			return Decision{Action: ForgetSet, Reason: ExtraSet + " is pending with no instances: a stale registration"}
		}
	case extra == nil && in.Instances > 0:
		if !in.LaunchedAt.IsZero() && now.Sub(in.LaunchedAt) > cfg.LaunchTimeout {
			return Decision{Action: CancelLaunch, Reason: fmt.Sprintf("%s's instances haven't registered after %s", ExtraSet, cfg.LaunchTimeout)}
		}
		return Decision{Action: None, Reason: ExtraSet + "'s instances are booting"}
	}

	scaledOut := extra != nil && extra.State == coordinator.StateActive
	if scaledOut {
		if !in.ScaleInOK {
			return Decision{Action: None, Reason: "3 sets; latency not below the thresholds for 10 minutes yet"}
		}
		if g := guard(now, in, cfg, true); g != "" {
			return Decision{Action: None, Reason: "scale-in due", Blocked: g}
		}
		// ADR-0032: the load that caused the scale-out must have gone too.
		if mg != nil && mg.Kind == coordinator.KindAdd && mg.Shard == ExtraSet && mg.TriggerRate > 0 {
			if limit := 0.75 * 2.0 / 3.0 * mg.TriggerRate; in.Rate >= limit {
				return Decision{Action: None, Reason: "scale-in due",
					Blocked: fmt.Sprintf("load still %.0f req/s, at or above %.0f (0.75 x 2/3 of %.0f at scale-out)", in.Rate, limit, mg.TriggerRate)}
			}
		}
		return Decision{Action: StartLeave, Reason: fmt.Sprintf("every shard below the thresholds for 10 minutes at %.0f req/s", in.Rate)}
	}

	if len(in.ScaleOutDue) == 0 {
		if len(in.CoordinatorBound) > 0 {
			return Decision{Action: None, Reason: fmt.Sprintf("coordinator-bound (%v): a new replica set wouldn't help", in.CoordinatorBound)}
		}
		return Decision{Action: None, Reason: "2 sets; no scale-out condition"}
	}
	if g := guard(now, in, cfg, false); g != "" {
		return Decision{Action: None, Reason: fmt.Sprintf("scale-out due (%v)", in.ScaleOutDue), Blocked: g}
	}
	return Decision{Action: LaunchSet, Reason: fmt.Sprintf("scale-out due for %v at %.0f req/s", in.ScaleOutDue, in.Rate), Rate: in.Rate}
}

// guard returns the first guard that blocks a scaling action, or "" (ADR-0032). For a
// scale-in, a replica of the leaving set that is down doesn't block, as long as that set
// still has a confirmed leader and a quorum: a damaged extra set can still be removed.
func guard(now time.Time, in Inputs, cfg Config, scaleIn bool) string {
	ring := map[string]bool{}
	for _, id := range in.Cluster.Ring {
		ring[id] = true
	}
	for _, s := range in.Cluster.Sets {
		if !ring[s.ID] {
			continue
		}
		if s.Leader == "" {
			return s.ID + " has no leader"
		}
		for _, r := range s.Replicas {
			if r.Available {
				continue
			}
			if scaleIn && s.ID == ExtraSet && s.Serving {
				continue
			}
			return fmt.Sprintf("replica %s of %s is down", r.ID, s.ID)
		}
	}
	if in.HoldsRecently {
		return fmt.Sprintf("a write hold ended within %s (p99 still carries it)", cfg.HoldWindow)
	}
	if mg := in.Cluster.Migration; mg != nil && !mg.Finished.IsZero() && now.Sub(mg.Finished) < cfg.Cooldown {
		return fmt.Sprintf("cooldown: migration %s finished %s ago (%s)", mg.ID, now.Sub(mg.Finished).Round(time.Second), cfg.Cooldown)
	}
	if !scaleIn && len(in.Cluster.Ring) >= 3 {
		return "upper bound: 3 replica sets"
	}
	return ""
}
