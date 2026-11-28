// Command kvops is the ops agent (ADR-0034, spec ops-agent).
//
//	kvops run       one run: Claude reads alerts, named metrics, the cluster state and the
//	                executor's decisions, and ends with one proposal, which is checked, saved
//	                under --store, recorded and posted to Alertmanager. Meant to run in its
//	                container (make ops-agent): no AWS credentials, no admin key, no approvals.
//	kvops approve ID   the gate: shows the proposal, checks it again, asks; a yes writes the
//	                approval the executor (kvscale --ops-dir) acts on. Run by the author, on
//	                the host, never in the agent's container.
//	kvops eval      the evaluation set (testdata/eval): recorded situations with the expected
//	                kind; prints each result and the pass rate. Calls the model: billed.
//
// Environment: ANTHROPIC_API_KEY (run, eval), KV_STATUS_KEY (an API key with only the status
// scope; run, approve).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/iravelmakina/capstone-kv/internal/ops"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	store := fs.String("store", "ops", "directory of proposals, runs, approvals, executions")
	prom := fs.String("prometheus", "http://localhost:9090", "Prometheus base URL")
	coord := fs.String("coordinator", "http://localhost:8080", "coordinator base URL")
	am := fs.String("alertmanager", "", "Alertmanager base URL to post the proposal to; empty = don't")
	execLog := fs.String("executor-log", "", "kvscale's JSON log, for its decisions")
	model := fs.String("model", defaultModel, "Claude model")
	evalDir := fs.String("eval-dir", "testdata/eval", "evaluation situations (eval)")
	approver := fs.String("approver", os.Getenv("USER"), "who approves (approve)")
	fs.Parse(args)
	ctx := context.Background()

	switch cmd {
	case "run":
		tb := ops.Toolbox{Prometheus: *prom, Coordinator: *coord, StatusKey: os.Getenv("KV_STATUS_KEY"), ExecutorLog: *execLog}
		id := "p-" + time.Now().UTC().Format("20060102-150405")
		client := anthropic.NewClient()
		run, err := runAgent(ctx, &client.Beta.Messages, tb, *model, id, time.Now)
		s := ops.Store{Dir: *store}
		if err != nil {
			run.Error = err.Error()
			s.SaveRun(id, run)
			fail("run %s: %v", id, err)
		}
		c, cerr := checkContext(ctx, *coord, os.Getenv("KV_STATUS_KEY"), *execLog)
		if cerr != nil {
			run.Proposal.Rejected = "could not read the state to check it: " + cerr.Error()
		} else {
			ops.Check(run.Proposal, c)
		}
		if err := s.SaveProposal(*run.Proposal); err != nil {
			fail("save proposal: %v", err)
		}
		s.SaveRun(id, run)
		fmt.Println(run.Proposal.Summary())
		if *am != "" {
			if err := notify(ctx, *am, *run.Proposal); err != nil {
				fmt.Fprintln(os.Stderr, "alertmanager:", err)
			}
		}
		if run.Proposal.Kind != ops.NoAction && run.Proposal.Rejected == "" {
			fmt.Printf("\nTo approve: make ops-approve ID=%s (within %s)\n", id, ops.Expiry)
		}

	case "approve":
		if fs.NArg() != 1 {
			usage()
		}
		c, err := checkContext(ctx, *coord, os.Getenv("KV_STATUS_KEY"), *execLog)
		if err != nil {
			fail("read the state: %v", err)
		}
		err = ops.Store{Dir: *store}.Approve(fs.Arg(0), *approver, c, time.Now(), func(summary string) bool {
			fmt.Println(summary)
			fmt.Print("\nApprove this proposal? Type yes: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			return strings.TrimSpace(line) == "yes"
		})
		if err != nil {
			fail("%v", err)
		}
		fmt.Println("approved; the executor (kvscale --ops-dir) acts on it after checking its guards again")

	case "eval":
		client := anthropic.NewClient()
		if err := runEval(ctx, &client.Beta.Messages, *model, *evalDir); err != nil {
			fail("%v", err)
		}

	default:
		usage()
	}
}

// checkContext reads what ops.Check needs: the cluster state (status key) and whether the
// executor runs with autoscaling on and what blocks it, from its log.
func checkContext(ctx context.Context, coordURL, key, execLog string) (ops.Context, error) {
	var c ops.Context
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(coordURL, "/")+"/v1/admin/cluster", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("cluster state: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c.Cluster); err != nil {
		return c, err
	}
	c.AutoscalerOn, c.AutoscalerHold = executorState(execLog)
	return c, nil
}

// executorState reads kvscale's log: autoscaling on (its last start line) and the guard of
// its last decision. No log: off.
func executorState(path string) (on bool, hold string) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return false, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["msg"] {
		case "started":
			on, _ = m["autoscale"].(bool)
		case "decision":
			hold, _ = m["blocked"].(string)
		}
	}
	return on, hold
}

// notify posts the proposal to Alertmanager as an info alert, so it reaches the chat.
func notify(ctx context.Context, am string, p ops.Proposal) error {
	summary := fmt.Sprintf("ops agent proposes %s %s%s: %s", p.Kind, p.Shard, map[bool]string{true: " -> " + p.To, false: ""}[p.To != ""], p.Reasoning)
	if p.Rejected != "" {
		summary += " (rejected by the checks: " + p.Rejected + ")"
	}
	body, _ := json.Marshal([]map[string]any{{
		"labels":      map[string]string{"alertname": "OpsAgentProposal", "severity": "info", "kind": p.Kind, "proposal": p.ID},
		"annotations": map[string]string{"summary": summary, "description": "Approve with make ops-approve ID=" + p.ID + " (ADR-0034)."},
		"startsAt":    time.Now().UTC().Format(time.RFC3339),
		"endsAt":      time.Now().Add(ops.Expiry).UTC().Format(time.RFC3339),
	}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(am, "/")+"/api/v2/alerts", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kvops run|approve ID|eval [flags]")
	os.Exit(2)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "kvops: "+format+"\n", a...)
	os.Exit(1)
}
