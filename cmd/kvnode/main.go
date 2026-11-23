// Command kvnode runs one node of capstone-kv: the coordinator or a shard (ADR-0009).
// One binary, one Docker image; the role comes from a flag or an environment variable,
// so the same image runs everywhere and only its configuration differs.
//
//	kvnode --role=coordinator --expected-shards=2
//	kvnode --role=shard --id=shard-1 --coordinator=http://10.20.0.10:8080 --advertise=http://10.20.64.11:8080
//
// Every flag can also be set as KV_<NAME> (e.g. KV_ROLE, KV_SHARD_ID), which is how the
// container gets its settings.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/coordinator"
	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// env returns the environment variable or a default; flags then override it.
func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func main() {
	role := flag.String("role", env("KV_ROLE", ""), "coordinator or shard")
	listen := flag.String("listen", env("KV_LISTEN", ":8080"), "address to serve HTTP on")
	dataDir := flag.String("data-dir", env("KV_DATA_DIR", "/data"), "directory for the append-only log")

	// Coordinator settings.
	expected := flag.Int("expected-shards", envInt("KV_EXPECTED_SHARDS", 2), "shards to wait for before serving items")
	vnodes := flag.Int("vnodes", envInt("KV_VNODES", ring.DefaultVirtualNodes), "virtual nodes per shard (ADR-0011)")
	timeout := flag.Duration("heartbeat-timeout", envDuration("KV_HEARTBEAT_TIMEOUT", 15*time.Second), "silence after which a shard is unavailable")

	// Shard settings.
	id := flag.String("id", env("KV_SHARD_ID", ""), "stable shard ID, e.g. shard-1")
	coordURL := flag.String("coordinator", env("KV_COORDINATOR_URL", ""), "coordinator base URL")
	advertise := flag.String("advertise", env("KV_ADVERTISE_URL", ""), "base URL the coordinator should call this shard on")
	interval := flag.Duration("heartbeat-interval", envDuration("KV_HEARTBEAT_INTERVAL", 5*time.Second), "how often to re-register")
	flag.Parse()

	// Stop cleanly on Ctrl-C or `docker stop` (SIGTERM): finish requests, close the log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var handler http.Handler
	var closeStore func() error
	switch *role {
	case "coordinator":
		tables, err := store.Open(filepath.Join(*dataDir, "tables"))
		if err != nil {
			log.Fatalf("open table registry: %v", err)
		}
		closeStore = tables.Close
		members := coordinator.NewMembership(ring.New(*vnodes), *timeout, *expected)
		handler = coordinator.New(tables, members).Handler()
		log.Printf("coordinator: expecting %d shards, %d virtual nodes each", *expected, *vnodes)

	case "shard":
		if *id == "" || *coordURL == "" || *advertise == "" {
			log.Fatal("shard needs --id, --coordinator and --advertise")
		}
		st, err := store.Open(filepath.Join(*dataDir, "shard"))
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		closeStore = st.Close
		handler = shard.Handler(st)
		go shard.RegisterLoop(ctx, *coordURL, shard.Registration{ID: *id, Address: *advertise}, *interval)
		log.Printf("shard %s: %d keys replayed, registering with %s as %s", *id, st.Len(), *coordURL, *advertise)

	default:
		log.Fatal("--role must be coordinator or shard")
	}

	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	if err := closeStore(); err != nil {
		log.Printf("close store: %v", err)
	}
}
