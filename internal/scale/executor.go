package scale

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
	"github.com/iravelmakina/capstone-kv/internal/ops"
)

// Metrics reads the monitor's Prometheus.
type Metrics interface {
	FiringAlerts(ctx context.Context) ([]Alert, error)
	Scalar(ctx context.Context, expr string) (float64, error) // NaN-free: 0 for an empty result
}

// Alert is a firing alert's name and labels.
type Alert struct {
	Name   string
	Labels map[string]string
}

// Cluster is the coordinator's admin API (admin key).
type Cluster interface {
	State(ctx context.Context) (coordinator.ClusterState, error)
	StartMigration(ctx context.Context, req coordinator.MigrationRequest) error
	Forget(ctx context.Context, shard string) error
	TransferLeader(ctx context.Context, shard, to string) error // approved ops-agent proposals
	AbortMigration(ctx context.Context) error
}

// Proposals are the ops agent's approved proposals (internal/ops.Store).
type Proposals interface {
	Approved() ([]ops.Proposal, []ops.Approval, error)
	MarkExecuted(id, outcome string, now time.Time) error
}

// Capacity starts and stops the extra replica set's instances.
type Capacity interface {
	Prepare(ctx context.Context) error // AWS: suspend the groups' own processes (ADR-0035)
	Instances(ctx context.Context) (int, error)
	Start(ctx context.Context) error // one instance per AZ
	Stop(ctx context.Context) error  // remove scale-in protection, then none
}

// Notifier tells people (Alertmanager, so Telegram).
type Notifier interface {
	Notify(ctx context.Context, d Decision) error
}

// Executor runs Decide every tick and carries out its action.
type Executor struct {
	Metrics  Metrics
	Cluster  Cluster
	Capacity Capacity
	Notifier Notifier
	Config   Config
	// AutoscaleOff: decide nothing on its own (scale-out and scale-in become "none"), but
	// finish what is under way and execute approved proposals (ADR-0034: the agent proposes
	// scaling only when the autoscaler is off or blocked).
	AutoscaleOff bool
	Proposals    Proposals // nil: no ops agent
	Now          func() time.Time
	Log          *slog.Logger

	launchedAt time.Time
	last       string // the last decision told, so a repeated one isn't sent again
	prepared   bool
}

// Queries the executor reads (ADR-0032).
const (
	rateQuery  = `sum(rate(kv_requests_total[10m]))`
	holdsQuery = `sum(increase(kv_write_hold_seconds_count[5m]))`
)

// Step gathers the inputs, decides, logs and notifies the decision when it changes, and acts.
func (e *Executor) Step(ctx context.Context) (Decision, error) {
	now := e.now()
	in, err := e.gather(ctx)
	if err != nil {
		return Decision{}, err
	}
	in.LaunchedAt = e.launchedAt
	d := Decide(now, in, e.Config)
	if e.AutoscaleOff && (d.Action == LaunchSet || d.Action == StartLeave) {
		d = Decision{Action: None, Reason: "autoscaling off (it would " + string(d.Action) + ": " + d.Reason + ")", Blocked: "autoscaling off"}
	}
	e.record(ctx, d)
	if err := e.act(ctx, d); err != nil {
		return d, err
	}
	if d.Action == None && e.Proposals != nil {
		return d, e.executeApproved(ctx, now, in, d)
	}
	return d, nil
}

// executeApproved carries out the oldest approved proposal, after checking it again
// against the current state and the scaling guards (ADR-0034: approval doesn't bypass a
// guard), and records the outcome either way.
func (e *Executor) executeApproved(ctx context.Context, now time.Time, in Inputs, d Decision) error {
	ps, as, err := e.Proposals.Approved()
	if err != nil || len(ps) == 0 {
		return err
	}
	p, a := ps[0], as[0]
	outcome := e.runProposal(ctx, now, in, d, p)
	e.log().Info("proposal", "id", p.ID, "kind", p.Kind, "approver", a.Approver, "outcome", outcome)
	if e.Notifier != nil {
		e.Notifier.Notify(ctx, Decision{Action: Action("proposal:" + p.Kind), Reason: p.ID + " (approved by " + a.Approver + "): " + outcome})
	}
	return e.Proposals.MarkExecuted(p.ID, outcome, now)
}

func (e *Executor) runProposal(ctx context.Context, now time.Time, in Inputs, d Decision, p ops.Proposal) string {
	if now.Sub(p.Created) > ops.Expiry+5*time.Minute {
		return "refused: expired"
	}
	hold := d.Blocked
	ops.Check(&p, ops.Context{Cluster: in.Cluster, AutoscalerOn: !e.AutoscaleOff, AutoscalerHold: hold})
	if p.Rejected != "" {
		return "refused: " + p.Rejected
	}
	cfg := e.Config.withDefaults()
	switch p.Kind {
	case ops.TransferLeader:
		if err := e.Cluster.TransferLeader(ctx, p.Shard, p.To); err != nil {
			return "failed: " + err.Error()
		}
	case ops.AbortMigration:
		if err := e.Cluster.AbortMigration(ctx); err != nil {
			return "failed: " + err.Error()
		}
	case ops.ScaleUp:
		if g := guard(now, in, cfg, false); g != "" {
			return "refused: " + g
		}
		if err := e.act(ctx, Decision{Action: LaunchSet, Rate: in.Rate}); err != nil {
			return "failed: " + err.Error()
		}
	case ops.ScaleDown:
		if g := guard(now, in, cfg, true); g != "" {
			return "refused: " + g
		}
		if err := e.act(ctx, Decision{Action: StartLeave}); err != nil {
			return "failed: " + err.Error()
		}
	default:
		return "refused: nothing to execute for " + p.Kind
	}
	return "executed"
}

// Run steps every interval until ctx ends; a failed step is logged and retried.
func (e *Executor) Run(ctx context.Context, every time.Duration) {
	for ctx.Err() == nil {
		if _, err := e.Step(ctx); err != nil {
			e.log().Error("step failed", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(every):
		}
	}
}

func (e *Executor) gather(ctx context.Context) (Inputs, error) {
	var in Inputs
	var err error
	if in.Cluster, err = e.Cluster.State(ctx); err != nil {
		return in, fmt.Errorf("cluster state: %w", err)
	}
	alerts, err := e.Metrics.FiringAlerts(ctx)
	if err != nil {
		return in, fmt.Errorf("alerts: %w", err)
	}
	for _, a := range alerts {
		switch a.Name {
		case "ScaleOutDue":
			in.ScaleOutDue = append(in.ScaleOutDue, a.Labels["shard"]+"/"+a.Labels["trigger"])
		case "ScaleInLatencyOk":
			in.ScaleInOK = true
		case "CoordinatorBound":
			in.CoordinatorBound = append(in.CoordinatorBound, a.Labels["shard"])
		}
	}
	if in.Rate, err = e.Metrics.Scalar(ctx, rateQuery); err != nil {
		return in, fmt.Errorf("request rate: %w", err)
	}
	holds, err := e.Metrics.Scalar(ctx, holdsQuery)
	if err != nil {
		return in, fmt.Errorf("write holds: %w", err)
	}
	in.HoldsRecently = holds > 0
	if in.Instances, err = e.Capacity.Instances(ctx); err != nil {
		return in, fmt.Errorf("instances: %w", err)
	}
	if in.Instances == 0 {
		e.launchedAt = time.Time{}
	}
	return in, nil
}

func (e *Executor) act(ctx context.Context, d Decision) error {
	switch d.Action {
	case LaunchSet:
		if !e.prepared {
			if err := e.Capacity.Prepare(ctx); err != nil {
				return fmt.Errorf("prepare capacity: %w", err)
			}
			e.prepared = true
		}
		if err := e.Capacity.Start(ctx); err != nil {
			return fmt.Errorf("start %s: %w", ExtraSet, err)
		}
		e.launchedAt = e.now()
	case StartJoin:
		return e.Cluster.StartMigration(ctx, coordinator.MigrationRequest{Add: ExtraSet, TriggerRate: d.Rate})
	case StartLeave:
		return e.Cluster.StartMigration(ctx, coordinator.MigrationRequest{Remove: ExtraSet})
	case StopSet, CancelLaunch:
		if err := e.Capacity.Stop(ctx); err != nil {
			return fmt.Errorf("stop %s: %w", ExtraSet, err)
		}
		e.launchedAt = time.Time{}
	case ForgetSet:
		return e.Cluster.Forget(ctx, ExtraSet)
	}
	return nil
}

// record logs every decision and notifies the ones that differ from the last told: a
// steady "no scale-out condition" every 15 s would bury the chat.
func (e *Executor) record(ctx context.Context, d Decision) {
	e.log().Info("decision", "action", string(d.Action), "reason", d.Reason, "blocked", d.Blocked, "rate", d.Rate)
	key := string(d.Action) + "|" + d.Reason + "|" + d.Blocked
	if key == e.last || (d.Action == None && d.Blocked == "") {
		e.last = key
		return
	}
	e.last = key
	if e.Notifier != nil {
		if err := e.Notifier.Notify(ctx, d); err != nil {
			e.log().Warn("notify failed", "err", err)
		}
	}
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
