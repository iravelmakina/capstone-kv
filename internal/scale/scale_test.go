package scale

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
	"github.com/iravelmakina/capstone-kv/internal/ops"
)

var t0 = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func set(id, state string, serving bool, down ...string) coordinator.ShardStatus {
	s := coordinator.ShardStatus{ID: id, State: state, Serving: serving, Leader: id[len(id)-1:] + "a"}
	for _, l := range []string{"a", "b", "c"} {
		r := coordinator.ReplicaStatus{ID: id[len(id)-1:] + l, Available: true}
		for _, d := range down {
			if d == r.ID {
				r.Available = false
			}
		}
		s.Replicas = append(s.Replicas, r)
	}
	return s
}

func twoSets(down ...string) coordinator.ClusterState {
	return coordinator.ClusterState{Ring: []string{"shard-1", "shard-2"},
		Sets: []coordinator.ShardStatus{set("shard-1", "active", true, down...), set("shard-2", "active", true)}}
}

func threeSets(extraDown ...string) coordinator.ClusterState {
	c := twoSets()
	c.Ring = append(c.Ring, "shard-3")
	c.Sets = append(c.Sets, set("shard-3", "active", true, extraDown...))
	c.Migration = &coordinator.Migration{ID: "add-shard-3-1", Kind: "add", Shard: "shard-3", Phase: "done",
		TriggerRate: 900, Finished: t0.Add(-time.Hour)}
	return c
}

// Spec autoscaling: every rule of ADR-0032's executor side, one case each.
func TestDecide(t *testing.T) {
	recent := twoSets()
	recent.Migration = &coordinator.Migration{ID: "remove-shard-3-1", Phase: "done", Finished: t0.Add(-3 * time.Minute)}
	running := twoSets()
	running.Migration = &coordinator.Migration{ID: "add-shard-3-2", Phase: "copying"}
	pending := twoSets()
	pending.Sets = append(pending.Sets, set("shard-3", "pending", false))
	pendingServing := twoSets()
	pendingServing.Sets = append(pendingServing.Sets, set("shard-3", "pending", true))
	retired := twoSets()
	retired.Sets = append(retired.Sets, set("shard-3", "retired", false))

	for _, tc := range []struct {
		name    string
		in      Inputs
		action  Action
		blocked string
	}{
		{"nothing to do", Inputs{Cluster: twoSets()}, None, ""},
		{"scale-out due", Inputs{Cluster: twoSets(), ScaleOutDue: []string{"shard-1/write"}, Rate: 900}, LaunchSet, ""},
		{"replica down blocks scale-out", Inputs{Cluster: twoSets("1b"), ScaleOutDue: []string{"shard-1/write"}}, None, "replica 1b of shard-1 is down"},
		{"recent hold blocks", Inputs{Cluster: twoSets(), ScaleOutDue: []string{"shard-1/write"}, HoldsRecently: true}, None, "write hold"},
		{"cooldown blocks", Inputs{Cluster: recent, ScaleOutDue: []string{"shard-1/write"}}, None, "cooldown"},
		{"migration running: hands off", Inputs{Cluster: running, ScaleOutDue: []string{"shard-1/write"}}, None, ""},
		{"coordinator-bound only", Inputs{Cluster: twoSets(), CoordinatorBound: []string{"shard-1"}}, None, ""},
		{"booting", Inputs{Cluster: twoSets(), Instances: 3, LaunchedAt: t0.Add(-time.Minute)}, None, ""},
		{"never registered", Inputs{Cluster: twoSets(), Instances: 3, LaunchedAt: t0.Add(-11 * time.Minute)}, CancelLaunch, ""},
		{"pending, not serving yet", Inputs{Cluster: pending, Instances: 3, LaunchedAt: t0.Add(-time.Minute)}, None, ""},
		{"pending and serving: join", Inputs{Cluster: pendingServing, Instances: 3, Rate: 850}, StartJoin, ""},
		{"stale pending", Inputs{Cluster: pending}, ForgetSet, ""},
		{"3 sets, latency still high", Inputs{Cluster: threeSets(), Rate: 100}, None, ""},
		{"scale-in", Inputs{Cluster: threeSets(), ScaleInOK: true, Rate: 400}, StartLeave, ""},
		{"load still high", Inputs{Cluster: threeSets(), ScaleInOK: true, Rate: 600}, None, "load still 600"},
		{"damaged extra set can leave", Inputs{Cluster: threeSets("3b"), ScaleInOK: true, Rate: 100}, StartLeave, ""},
		{"base replica down blocks scale-in", Inputs{Cluster: func() coordinator.ClusterState {
			c := threeSets()
			c.Sets[0] = set("shard-1", "active", true, "1c")
			return c
		}(), ScaleInOK: true, Rate: 100}, None, "replica 1c"},
		{"retired, still running", Inputs{Cluster: retired, Instances: 3}, StopSet, ""},
		{"retired and stopped", Inputs{Cluster: retired}, ForgetSet, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(t0, tc.in, Config{})
			if d.Action != tc.action || !strings.Contains(d.Blocked, tc.blocked) || (tc.blocked == "" && d.Blocked != "") {
				t.Fatalf("got %+v, want action %s blocked %q", d, tc.action, tc.blocked)
			}
		})
	}
	// ADR-0032: 0.75 x (n-1)/n of the rate at scale-out, with n = 3: half of 900.
	if d := Decide(t0, Inputs{Cluster: threeSets(), ScaleInOK: true, Rate: 449}, Config{}); d.Action != StartLeave {
		t.Fatalf("449 < 450 should scale in: %+v", d)
	}
	if d := Decide(t0, Inputs{Cluster: threeSets(), ScaleInOK: true, Rate: 450}, Config{}); d.Action != None {
		t.Fatalf("450 >= 450 should not scale in: %+v", d)
	}
}

// --- an executor against fakes ------------------------------------------------------------

type world struct {
	cluster   coordinator.ClusterState
	alerts    []Alert
	rate      float64
	instances int
	actions   []string
	notified  []string
}

func (w *world) FiringAlerts(context.Context) ([]Alert, error) { return w.alerts, nil }
func (w *world) Scalar(_ context.Context, expr string) (float64, error) {
	if expr == rateQuery {
		return w.rate, nil
	}
	return 0, nil
}
func (w *world) State(context.Context) (coordinator.ClusterState, error) { return w.cluster, nil }
func (w *world) StartMigration(_ context.Context, r coordinator.MigrationRequest) error {
	if r.Add != "" {
		w.actions = append(w.actions, "join")
		w.cluster.Migration = &coordinator.Migration{ID: "add", Kind: "add", Shard: r.Add, Phase: "copying", TriggerRate: r.TriggerRate}
	} else {
		w.actions = append(w.actions, "leave")
		w.cluster.Migration = &coordinator.Migration{ID: "remove", Kind: "remove", Shard: r.Remove, Phase: "copying"}
	}
	return nil
}
func (w *world) Forget(context.Context, string) error {
	w.actions = append(w.actions, "forget")
	w.cluster.Sets = w.cluster.Sets[:2]
	return nil
}
func (w *world) Prepare(context.Context) error          { w.actions = append(w.actions, "prepare"); return nil }
func (w *world) Instances(context.Context) (int, error) { return w.instances, nil }
func (w *world) TransferLeader(_ context.Context, shard, to string) error {
	w.actions = append(w.actions, "transfer "+shard+" "+to)
	return nil
}
func (w *world) AbortMigration(context.Context) error {
	w.actions = append(w.actions, "abort")
	return nil
}
func (w *world) Start(context.Context) error {
	w.actions = append(w.actions, "start")
	w.instances = 3
	return nil
}
func (w *world) Stop(context.Context) error {
	w.actions = append(w.actions, "stop")
	w.instances = 0
	return nil
}
func (w *world) Notify(_ context.Context, d Decision) error {
	w.notified = append(w.notified, string(d.Action))
	return nil
}

// The whole cycle, a tick at a time, with the executor replaced by a new one in the middle
// (as if restarted): everything it needs is in the cluster's state.
func TestExecutorCycle(t *testing.T) {
	w := &world{cluster: twoSets(), rate: 900}
	now := t0
	newExec := func() *Executor {
		return &Executor{Metrics: w, Cluster: w, Capacity: w, Notifier: w, Now: func() time.Time { return now }}
	}
	e := newExec()
	step := func(want Action) {
		t.Helper()
		d, err := e.Step(context.Background())
		if err != nil || d.Action != want {
			t.Fatalf("at %s: %+v %v, want %s (actions so far %v)", now.Sub(t0), d, err, want, w.actions)
		}
		now = now.Add(15 * time.Second)
	}
	step(None)
	w.alerts = []Alert{{Name: "ScaleOutDue", Labels: map[string]string{"shard": "shard-1", "trigger": "write"}}}
	step(LaunchSet)
	step(None) // booting
	w.cluster.Sets = append(w.cluster.Sets, set("shard-3", "pending", false))
	step(None) // registered, choosing its leader
	w.cluster.Sets[2].Serving = true
	e = newExec() // restart: nothing lost
	step(StartJoin)
	if w.cluster.Migration.TriggerRate != 900 {
		t.Fatalf("join recorded rate %v", w.cluster.Migration.TriggerRate)
	}
	step(None) // migration running
	w.cluster.Migration.Phase, w.cluster.Migration.Finished = "done", now
	w.cluster.Ring = append(w.cluster.Ring, "shard-3")
	w.cluster.Sets[2].State = "active"
	w.alerts = []Alert{{Name: "ScaleInLatencyOk"}}
	w.rate = 300
	step(None) // cooldown
	now = now.Add(10 * time.Minute)
	step(StartLeave)
	w.cluster.Migration.Phase, w.cluster.Migration.Finished = "done", now
	w.cluster.Ring = w.cluster.Ring[:2]
	w.cluster.Sets[2].State = "retired"
	step(StopSet)
	step(ForgetSet)
	step(None)
	if got := strings.Join(w.actions, ","); got != "prepare,start,join,leave,stop,forget" {
		t.Fatalf("actions %s", got)
	}
	if len(w.notified) < 5 {
		t.Fatalf("notified only %v", w.notified)
	}
}

// --- clients ------------------------------------------------------------------------------

// The AWS CLI calls, exactly: processes suspended, desired capacity 1 per group, and on the
// way down protection removed before desired capacity 0.
func TestASGCommands(t *testing.T) {
	var calls []string
	described := `{"AutoScalingGroups":[{"AutoScalingGroupName":"g-3a","Instances":[{"InstanceId":"i-1","LifecycleState":"InService"}]},
	  {"AutoScalingGroupName":"g-3b","Instances":[{"InstanceId":"i-2","LifecycleState":"Pending"}]},{"AutoScalingGroupName":"g-3c","Instances":[]}]}`
	a := ASGs{Groups: []string{"g-3a", "g-3b", "g-3c"}, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[1] == "describe-auto-scaling-groups" {
			return []byte(described), nil
		}
		return nil, nil
	}}
	ctx := context.Background()
	a.Prepare(ctx)
	if n, _ := a.Instances(ctx); n != 2 {
		t.Fatalf("instances %d, want 2", n)
	}
	calls = nil
	a.Start(ctx)
	if len(calls) != 3 || !strings.Contains(calls[0], "set-desired-capacity --auto-scaling-group-name g-3a --desired-capacity 1") {
		t.Fatalf("start: %v", calls)
	}
	calls = nil
	a.Stop(ctx)
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--no-protected-from-scale-in --instance-ids i-1") ||
		strings.Index(joined, "set-instance-protection") > strings.Index(joined, "--desired-capacity 0") {
		t.Fatalf("stop must unprotect before desired 0:\n%s", joined)
	}
}

func TestPrometheusAndAlertmanager(t *testing.T) {
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/alerts":
			io.WriteString(w, `{"data":{"alerts":[{"labels":{"alertname":"ScaleOutDue","shard":"shard-1"},"state":"firing"},{"labels":{"alertname":"ScaleInLatencyOk"},"state":"pending"}]}}`)
		case "/api/v1/query":
			io.WriteString(w, `{"data":{"result":[{"value":[1,"812.5"]}]}}`)
		case "/api/v2/alerts":
			json.NewDecoder(r.Body).Decode(&posted)
		}
	}))
	defer srv.Close()
	p := Prometheus{URL: srv.URL}
	alerts, err := p.FiringAlerts(context.Background())
	if err != nil || len(alerts) != 1 || alerts[0].Name != "ScaleOutDue" {
		t.Fatalf("alerts %v %v (pending ones must not count)", alerts, err)
	}
	if v, _ := p.Scalar(context.Background(), rateQuery); v != 812.5 {
		t.Fatalf("scalar %v", v)
	}
	if err := (Alertmanager{URL: srv.URL}).Notify(context.Background(), Decision{Action: LaunchSet, Reason: "x"}); err != nil || len(posted) != 1 {
		t.Fatalf("notify: %v %v", err, posted)
	}
}

// --- approved ops-agent proposals (ADR-0034) ----------------------------------------------

type proposals struct {
	ps       []ops.Proposal
	executed map[string]string
}

func (p *proposals) Approved() ([]ops.Proposal, []ops.Approval, error) {
	var ps []ops.Proposal
	var as []ops.Approval
	for _, x := range p.ps {
		if _, done := p.executed[x.ID]; !done {
			ps, as = append(ps, x), append(as, ops.Approval{ID: x.ID, Approver: "ira"})
		}
	}
	return ps, as, nil
}
func (p *proposals) MarkExecuted(id, outcome string, _ time.Time) error {
	p.executed[id] = outcome
	return nil
}

// Spec ops-agent "Approved and executed", "State changed after proposal", "Expired
// proposal"; and with autoscaling off the executor decides nothing itself.
func TestApprovedProposals(t *testing.T) {
	w := &world{cluster: twoSets(), rate: 900}
	pr := &proposals{executed: map[string]string{}}
	e := &Executor{Metrics: w, Cluster: w, Capacity: w, Notifier: w, Proposals: pr, AutoscaleOff: true, Now: func() time.Time { return t0 }}

	pr.ps = []ops.Proposal{{ID: "p-1", Created: t0.Add(-time.Minute), Kind: ops.TransferLeader, Shard: "shard-1", To: "1b", Reasoning: "colocated"}}
	e.Step(context.Background())
	if pr.executed["p-1"] != "executed" || !strings.Contains(strings.Join(w.actions, ","), "transfer shard-1 1b") {
		t.Fatalf("transfer: %v %v", pr.executed, w.actions)
	}

	w.alerts = []Alert{{Name: "ScaleOutDue", Labels: map[string]string{"shard": "shard-1"}}}
	if d, _ := e.Step(context.Background()); d.Action != None || d.Blocked != "autoscaling off" {
		t.Fatalf("autoscaling off still decided: %+v", d)
	}
	w.cluster = twoSets("1c") // a replica went down after the proposal was made
	pr.ps = append(pr.ps, ops.Proposal{ID: "p-2", Created: t0.Add(-time.Minute), Kind: ops.ScaleUp, Blocker: "off", Reasoning: "load"})
	e.Step(context.Background())
	if !strings.Contains(pr.executed["p-2"], "refused: replica 1c of shard-1 is down") {
		t.Fatalf("state changed: %q", pr.executed["p-2"])
	}
	w.cluster = twoSets()
	pr.ps = append(pr.ps, ops.Proposal{ID: "p-3", Created: t0.Add(-16 * time.Minute), Kind: ops.ScaleUp, Blocker: "off", Reasoning: "load"})
	e.Step(context.Background())
	if pr.executed["p-3"] != "refused: expired" {
		t.Fatalf("expired: %q", pr.executed["p-3"])
	}
	pr.ps = append(pr.ps, ops.Proposal{ID: "p-4", Created: t0.Add(-time.Minute), Kind: ops.ScaleUp, Blocker: "off", Reasoning: "load"})
	e.Step(context.Background())
	if pr.executed["p-4"] != "executed" || w.instances != 3 {
		t.Fatalf("approved scale_up: %q, %d instances", pr.executed["p-4"], w.instances)
	}
}
