// Command kvscale is the autoscaling executor (ADR-0032, ADR-0035): every 15 s it reads the
// monitor's Prometheus (scaling alerts, request rate, write holds), the coordinator's
// cluster state and the extra replica set's instances, decides (internal/scale.Decide) and
// acts: start shard-3's instances, migrate it in, migrate it out, stop it, forget it. Each
// decision is logged as a JSON line and posted to Alertmanager (Telegram).
//
// It runs on the author's laptop during a lab session: nothing in the sandbox can hold the
// credentials to change capacity (ADR-0035). The admin key comes from the environment
// (KV_API_KEY, `make autoscale` reads it from Secrets Manager) and is never logged.
//
//	kvscale --coordinator http://<ip>:8080 --capacity aws --groups velmakina-kv-app-shard-3a,...
//	kvscale --coordinator http://localhost:8080 --capacity compose      (the local cluster)
//	kvscale ... --dry-run                                                (print, don't act)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
	"github.com/iravelmakina/capstone-kv/internal/ops"
	"github.com/iravelmakina/capstone-kv/internal/scale"
)

func main() {
	coord := flag.String("coordinator", "", "coordinator base URL (admin API)")
	prom := flag.String("prometheus", "http://localhost:9090", "Prometheus base URL (make prometheus forwards it in AWS)")
	am := flag.String("alertmanager", "http://localhost:9093", "Alertmanager base URL; empty = don't notify")
	capacity := flag.String("capacity", "aws", "aws (the Auto Scaling groups) or compose (the local shard3 services)")
	groups := flag.String("groups", "velmakina-kv-app-shard-3a,velmakina-kv-app-shard-3b,velmakina-kv-app-shard-3c", "the extra set's groups")
	dir := flag.String("compose-dir", ".", "repository with docker-compose.yml (capacity compose)")
	every := flag.Duration("every", 15*time.Second, "how often to decide")
	once := flag.Bool("once", false, "decide once and exit")
	dryRun := flag.Bool("dry-run", false, "print the capacity commands and admin calls instead of running them")
	autoscale := flag.Bool("autoscale", true, "decide scaling itself; false = only finish what runs and execute approved proposals")
	opsDir := flag.String("ops-dir", "", "the ops agent's store (proposals, approvals); empty = no agent (ADR-0034)")
	flag.Parse()
	if *coord == "" {
		log.Fatal("--coordinator is required")
	}
	key := os.Getenv("KV_API_KEY")
	if key == "" {
		log.Fatal("KV_API_KEY (a key with the admin scope) must be set")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("app", "kvscale")
	slog.SetDefault(logger)

	run := scale.Exec
	if *dryRun {
		run = func(_ context.Context, name string, args ...string) ([]byte, error) {
			logger.Info("dry-run: would run", "cmd", name+" "+strings.Join(args, " "))
			if len(args) > 1 && args[1] == "describe-auto-scaling-groups" {
				return []byte(`{"AutoScalingGroups":[]}`), nil
			}
			return nil, nil
		}
	}
	var capa scale.Capacity
	switch *capacity {
	case "aws":
		capa = scale.ASGs{Groups: strings.Split(*groups, ","), Run: run}
	case "compose":
		capa = scale.Compose{Dir: *dir, Run: run}
	default:
		log.Fatalf("--capacity must be aws or compose, not %q", *capacity)
	}
	var cluster scale.Cluster = scale.Coordinator{URL: *coord, Key: key}
	if *dryRun {
		cluster = dryCluster{cluster, logger}
	}
	e := &scale.Executor{Metrics: scale.Prometheus{URL: *prom}, Cluster: cluster, Capacity: capa, Log: logger,
		AutoscaleOff: !*autoscale}
	if *opsDir != "" {
		e.Proposals = ops.Store{Dir: *opsDir}
	}
	if *am != "" && !*dryRun {
		e.Notifier = scale.Alertmanager{URL: *am}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		d, err := e.Step(ctx)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %s %s\n", d.Action, d.Reason, d.Blocked)
		return
	}
	logger.Info("started", "coordinator", *coord, "capacity", *capacity, "every", every.String(), "dry_run", *dryRun,
		"autoscale", *autoscale, "ops_dir", *opsDir)
	e.Run(ctx, *every)
}

// dryCluster reads the real state but only logs the changes it would make.
type dryCluster struct {
	scale.Cluster
	log *slog.Logger
}

func (d dryCluster) StartMigration(_ context.Context, req coordinator.MigrationRequest) error {
	d.log.Info("dry-run: would start a migration", "add", req.Add, "remove", req.Remove, "trigger_rate", req.TriggerRate)
	return nil
}

func (d dryCluster) TransferLeader(_ context.Context, shard, to string) error {
	d.log.Info("dry-run: would transfer", "shard", shard, "to", to)
	return nil
}

func (d dryCluster) AbortMigration(context.Context) error {
	d.log.Info("dry-run: would abort the migration")
	return nil
}

func (d dryCluster) Forget(_ context.Context, shard string) error {
	d.log.Info("dry-run: would forget", "shard", shard)
	return nil
}
