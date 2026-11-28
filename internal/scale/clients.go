package scale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
)

// --- Prometheus ------------------------------------------------------------------------

// Prometheus is the HTTP API of the monitor's Prometheus (through SSM port forwarding in
// AWS, localhost:9090 locally).
type Prometheus struct {
	URL    string
	Client *http.Client
}

func (p Prometheus) get(ctx context.Context, path string, q url.Values, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+path+"?"+q.Encode(), nil)
	resp, err := client(p.Client).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %d %s", path, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// FiringAlerts lists the alerts Prometheus has firing (not pending).
func (p Prometheus) FiringAlerts(ctx context.Context) ([]Alert, error) {
	var body struct {
		Data struct {
			Alerts []struct {
				Labels map[string]string `json:"labels"`
				State  string            `json:"state"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/api/v1/alerts", nil, &body); err != nil {
		return nil, err
	}
	var out []Alert
	for _, a := range body.Data.Alerts {
		if a.State == "firing" {
			out = append(out, Alert{Name: a.Labels["alertname"], Labels: a.Labels})
		}
	}
	return out, nil
}

// Scalar runs an instant query and returns its single value, 0 for an empty result or NaN.
func (p Prometheus) Scalar(ctx context.Context, expr string) (float64, error) {
	var body struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/api/v1/query", url.Values{"query": {expr}}, &body); err != nil {
		return 0, err
	}
	if len(body.Data.Result) == 0 {
		return 0, nil
	}
	s, _ := body.Data.Result[0].Value[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, nil
	}
	return v, nil
}

// --- the coordinator ---------------------------------------------------------------------

// Coordinator is the admin API, called with a key that has the admin scope.
type Coordinator struct {
	URL    string
	Key    string // from the environment (Secrets Manager in AWS); never logged
	Client *http.Client
}

func (c Coordinator) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, r)
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	return client(c.Client).Do(req)
}

// State is GET /v1/admin/cluster.
func (c Coordinator) State(ctx context.Context) (coordinator.ClusterState, error) {
	var cs coordinator.ClusterState
	resp, err := c.do(ctx, http.MethodGet, "/v1/admin/cluster", nil)
	if err != nil {
		return cs, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return cs, fmt.Errorf("cluster: %d %s", resp.StatusCode, b)
	}
	return cs, json.NewDecoder(resp.Body).Decode(&cs)
}

// StartMigration is POST /v1/admin/migrations.
func (c Coordinator) StartMigration(ctx context.Context, req coordinator.MigrationRequest) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/admin/migrations", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("start migration: %d %s", resp.StatusCode, b)
	}
	return nil
}

// Forget is DELETE /internal/v1/shards/{id} (a retired or stale pending set).
func (c Coordinator) Forget(ctx context.Context, shard string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/internal/v1/shards/"+url.PathEscape(shard), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("forget %s: %d %s", shard, resp.StatusCode, b)
	}
	return nil
}

// TransferLeader is POST /v1/admin/shards/{id}/leader (an approved proposal, ADR-0036).
func (c Coordinator) TransferLeader(ctx context.Context, shard, to string) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/admin/shards/"+url.PathEscape(shard)+"/leader", map[string]string{"to": to})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("transfer %s to %s: %d %s", shard, to, resp.StatusCode, b)
	}
	return nil
}

// AbortMigration is DELETE /v1/admin/migrations/current.
func (c Coordinator) AbortMigration(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/admin/migrations/current", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("abort migration: %d %s", resp.StatusCode, b)
	}
	return nil
}

// --- Alertmanager --------------------------------------------------------------------------

// Alertmanager posts each decision as a short-lived info alert, so it reaches Telegram.
type Alertmanager struct {
	URL    string
	Client *http.Client
	Now    func() time.Time
}

// Notify posts the decision.
func (a Alertmanager) Notify(ctx context.Context, d Decision) error {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	summary := "kvscale: " + string(d.Action) + ": " + d.Reason
	if d.Blocked != "" {
		summary = "kvscale: " + d.Reason + ", blocked: " + d.Blocked
	}
	body := []map[string]any{{
		"labels":      map[string]string{"alertname": "KvscaleDecision", "severity": "info", "action": string(d.Action)},
		"annotations": map[string]string{"summary": summary, "description": "Autoscaling executor decision (ADR-0032, ADR-0035)."},
		"startsAt":    now().UTC().Format(time.RFC3339),
		"endsAt":      now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
	}}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+"/api/v2/alerts", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client(a.Client).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("alertmanager: %d", resp.StatusCode)
	}
	return nil
}

// --- capacity: AWS Auto Scaling groups, through the AWS CLI --------------------------------

// Runner runs a command and returns its output (the AWS CLI, docker compose); tests and
// --dry-run swap it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs the command for real.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ASGs is the extra set's three single-subnet groups (ADR-0035), driven with the author's
// lab credentials (the AWS CLI's profile and region come from the environment).
type ASGs struct {
	Groups []string // e.g. velmakina-kv-app-shard-3a, -3b, -3c
	Run    Runner
}

// Prepare suspends the processes that would let a group terminate or replace on its own.
func (a ASGs) Prepare(ctx context.Context) error {
	for _, g := range a.Groups {
		if _, err := a.Run(ctx, "aws", "autoscaling", "suspend-processes", "--auto-scaling-group-name", g,
			"--scaling-processes", "ReplaceUnhealthy", "AZRebalance"); err != nil {
			return err
		}
	}
	return nil
}

type asgInstance struct {
	InstanceId     string
	LifecycleState string
}

func (a ASGs) describe(ctx context.Context) (map[string][]asgInstance, error) {
	args := append([]string{"autoscaling", "describe-auto-scaling-groups", "--auto-scaling-group-names"}, a.Groups...)
	args = append(args, "--output", "json")
	out, err := a.Run(ctx, "aws", args...)
	if err != nil {
		return nil, err
	}
	var body struct {
		AutoScalingGroups []struct {
			AutoScalingGroupName string
			Instances            []asgInstance
		}
	}
	if err := json.Unmarshal(out, &body); err != nil {
		return nil, err
	}
	m := map[string][]asgInstance{}
	for _, g := range body.AutoScalingGroups {
		m[g.AutoScalingGroupName] = g.Instances
	}
	return m, nil
}

// Instances counts the groups' instances that are pending or in service.
func (a ASGs) Instances(ctx context.Context) (int, error) {
	m, err := a.describe(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, is := range m {
		for _, i := range is {
			if strings.HasPrefix(i.LifecycleState, "Pending") || i.LifecycleState == "InService" {
				n++
			}
		}
	}
	return n, nil
}

// Start sets every group's desired capacity to 1.
func (a ASGs) Start(ctx context.Context) error { return a.setDesired(ctx, 1) }

// Stop removes the instances' scale-in protection (it would keep them alive), then sets
// every group's desired capacity to 0.
func (a ASGs) Stop(ctx context.Context) error {
	m, err := a.describe(ctx)
	if err != nil {
		return err
	}
	for g, is := range m {
		var ids []string
		for _, i := range is {
			ids = append(ids, i.InstanceId)
		}
		if len(ids) == 0 {
			continue
		}
		args := append([]string{"autoscaling", "set-instance-protection", "--auto-scaling-group-name", g, "--no-protected-from-scale-in", "--instance-ids"}, ids...)
		if _, err := a.Run(ctx, "aws", args...); err != nil {
			return err
		}
	}
	return a.setDesired(ctx, 0)
}

func (a ASGs) setDesired(ctx context.Context, n int) error {
	for _, g := range a.Groups {
		if _, err := a.Run(ctx, "aws", "autoscaling", "set-desired-capacity", "--auto-scaling-group-name", g,
			"--desired-capacity", strconv.Itoa(n)); err != nil {
			return err
		}
	}
	return nil
}

// --- capacity: the local Compose services ----------------------------------------------------

// Compose starts and stops r3a, r3b, r3c of the local cluster (profile shard3), so the
// executor can be run end to end without AWS.
type Compose struct {
	Dir string // the repository, where docker-compose.yml is
	Run Runner
}

func (c Compose) compose(ctx context.Context, args ...string) ([]byte, error) {
	return c.Run(ctx, "docker", append([]string{"compose", "--project-directory", c.Dir, "--profile", "shard3"}, args...)...)
}

// Prepare does nothing locally.
func (Compose) Prepare(context.Context) error { return nil }

// Instances counts the running shard-3 containers.
func (c Compose) Instances(ctx context.Context) (int, error) {
	out, err := c.compose(ctx, "ps", "--status", "running", "--services", "r3a", "r3b", "r3c")
	if err != nil {
		return 0, err
	}
	return len(strings.Fields(string(out))), nil
}

// Start starts the three containers.
func (c Compose) Start(ctx context.Context) error {
	_, err := c.compose(ctx, "up", "-d", "r3a", "r3b", "r3c")
	return err
}

// Stop stops them.
func (c Compose) Stop(ctx context.Context) error {
	_, err := c.compose(ctx, "stop", "r3a", "r3b", "r3c")
	return err
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 15 * time.Second}
}
