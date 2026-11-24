// Command kvbench runs YCSB workloads against capstone-kv with go-ycsb's core workload,
// client and measurements, and only our binding (ADR-0016). go-ycsb's own command bundles
// twenty database drivers and their dependencies; this one has exactly one.
//
//	kvbench load -P workloads/workloada -p kv.url=http://10.20.12.80:8080 -threads 16
//	kvbench run  -P workloads/workloada -p kv.url=http://10.20.12.80:8080 -threads 16
//
// "load" inserts recordcount records; "run" executes operationcount operations in the
// workload's mix. Both print go-ycsb's summary table (throughput, avg, p50-p99.99).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/client"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	_ "github.com/pingcap/go-ycsb/pkg/workload" // registers the "core" workload
	"github.com/pingcap/go-ycsb/pkg/ycsb"

	_ "github.com/iravelmakina/capstone-kv/bench/capstonekv" // registers "capstonekv"
)

// multi collects a repeatable flag (-P a -P b).
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "load" && os.Args[1] != "run") {
		fmt.Fprintln(os.Stderr, "usage: kvbench load|run -P <workload file> [-p name=value]... [-threads n]")
		os.Exit(2)
	}
	command := os.Args[1]
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	var files, values multi
	fs.Var(&files, "P", "workload property file (repeatable)")
	fs.Var(&values, "p", "property name=value (repeatable, overrides the files)")
	threads := fs.Int("threads", 0, "client threads (overrides the threadcount property)")
	fs.Parse(os.Args[2:])

	// Properties: files first, then -p overrides, then what the command implies.
	props := properties.NewProperties()
	if len(files) > 0 {
		props = properties.MustLoadFiles(files, properties.UTF8, false)
	}
	for _, kv := range values {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			log.Fatalf("bad property %q, want name=value", kv)
		}
		props.Set(name, value)
	}
	if *threads > 0 {
		props.Set(prop.ThreadCount, strconv.Itoa(*threads))
	}
	props.Set(prop.Command, command)
	props.Set(prop.DoTransactions, strconv.FormatBool(command == "run"))

	measurement.InitMeasure(props)
	workloadName := props.GetString(prop.Workload, "core")
	workload, err := ycsb.GetWorkloadCreator(workloadName).Create(props)
	if err != nil {
		log.Fatalf("create workload %s: %v", workloadName, err)
	}
	db, err := ycsb.GetDBCreator("capstonekv").Create(props)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	// DbWrapper times every call: that's where the latency numbers come from.
	wrapped := client.DbWrapper{DB: db}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Println("***************** properties *****************")
	for k, v := range props.Map() {
		fmt.Printf("%q=%q\n", k, v)
	}
	fmt.Println("**********************************************")
	start := time.Now()
	client.NewClient(props, workload, wrapped).Run(ctx)
	fmt.Println("**********************************************")
	fmt.Printf("%s finished, takes %s\n", command, time.Since(start).Round(time.Millisecond))
	measurement.Output()

	wrapped.Close()
	workload.Close()
}
