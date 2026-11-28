// Command kvnode runs one node of capstone-kv: the coordinator or a shard replica
// (ADR-0009, ADR-0020). One binary, one Docker image; the role comes from a flag or an
// environment variable, so the same image runs everywhere and only its configuration differs.
//
//	kvnode --role=coordinator --expected-shards=2 --leaders=shard-1=1a,shard-2=2b
//	kvnode --role=shard --shard=shard-1 --replica=1a --coordinator=http://10.20.0.10:8080 --advertise=http://10.20.64.11:8081
//
// Every flag can also be set as KV_<NAME> (e.g. KV_ROLE, KV_SHARD, KV_REPLICA), which is
// how the container gets its settings.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/auth"
	"github.com/iravelmakina/capstone-kv/internal/coordinator"
	"github.com/iravelmakina/capstone-kv/internal/health"
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
	metricsListen := flag.String("metrics-listen", env("KV_METRICS_LISTEN", ":9180"), "address to serve /metrics on, apart from the API (ADR-0025)")
	dataDir := flag.String("data-dir", env("KV_DATA_DIR", "/data"), "directory for the append-only log")

	w := flag.Int("w", envInt("KV_W", 2), "replicas a write needs before it commits: 1 or 2 (ADR-0021)")

	// Coordinator settings.
	expected := flag.Int("expected-shards", envInt("KV_EXPECTED_SHARDS", 2), "replica sets to wait for before serving items")
	replicas := flag.Int("replicas", envInt("KV_REPLICAS", 3), "replicas per shard, N")
	leaders := flag.String("leaders", env("KV_LEADERS", ""), "first leader per shard, e.g. shard-1=1a,shard-2=2b")
	vnodes := flag.Int("vnodes", envInt("KV_VNODES", ring.DefaultVirtualNodes), "virtual nodes per shard (ADR-0011)")
	apiKeys := flag.String("api-keys-file", env("KV_API_KEYS_FILE", ""), "API key hashes and scopes (ADR-0027); empty = no keys required")
	timeout := flag.Duration("heartbeat-timeout", envDuration("KV_HEARTBEAT_TIMEOUT", 15*time.Second), "silence after which a replica is unavailable")
	firstWait := flag.Duration("first-leader-wait", envDuration("KV_FIRST_LEADER_WAIT", 3*time.Minute), "how long a new set waits for all its replicas before choosing its first leader from a majority (ADR-0036)")

	// Replica settings.
	shardID := flag.String("shard", env("KV_SHARD", ""), "replica set this replica belongs to, e.g. shard-1")
	replicaID := flag.String("replica", env("KV_REPLICA", ""), "stable replica ID, e.g. 1a")
	coordURL := flag.String("coordinator", env("KV_COORDINATOR_URL", ""), "coordinator base URL")
	advertise := flag.String("advertise", env("KV_ADVERTISE_URL", ""), "base URL the coordinator and other replicas call this replica on")
	interval := flag.Duration("heartbeat-interval", envDuration("KV_HEARTBEAT_INTERVAL", 5*time.Second), "how often to report to the coordinator")
	flag.Parse()
	if *w != 1 && *w != 2 {
		log.Fatal("--w must be 1 or 2")
	}

	// JSON logs (ADR-0029). SetDefault also sends the log package's lines through this
	// handler, so every line of the process is JSON with its role and identity.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("role", *role)
	if *role == "shard" {
		logger = logger.With("shard", *shardID, "replica", *replicaID)
	}
	slog.SetDefault(logger)

	// Stop cleanly on Ctrl-C or `docker stop` (SIGTERM): finish requests, close the log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Listen before opening the logs: replaying a long log takes a while, and meanwhile the
	// node answers /health/live (200), /version and /health/ready (503 "replaying") instead
	// of refusing connections (M5, spec health-endpoints). The real handler replaces it
	// once the logs are replayed.
	api := health.NewSwap(health.Starting(health.NewVersion(*role, *shardID, *replicaID)))
	srv := &http.Server{Addr: *listen, Handler: api, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	var handler, metricsHandler http.Handler
	var closeStore func() error
	switch *role {
	case "coordinator":
		tables, err := store.Open(filepath.Join(*dataDir, "tables"))
		if err != nil {
			log.Fatalf("open table registry: %v", err)
		}
		leaderStore, err := store.Open(filepath.Join(*dataDir, "leaders"))
		if err != nil {
			log.Fatalf("open leader assignments: %v", err)
		}
		closeStore = func() error { leaderStore.Close(); return tables.Close() }
		members, err := coordinator.NewMembership(ring.New(*vnodes), coordinator.MembershipConfig{
			Timeout: *timeout, ExpectedShards: *expected, Replicas: *replicas,
			Leaders: leaderStore, Preferred: parsePairs(*leaders), FirstLeaderWait: *firstWait,
		})
		if err != nil {
			log.Fatalf("load leader assignments: %v", err)
		}
		go members.Watch(ctx, time.Second)
		// R + W > N: with W = 2 two replicas answer each read, with W = 1 all of them (ADR-0021).
		r := *replicas + 1 - *w
		coord := coordinator.New(tables, members, r)
		if *apiKeys != "" {
			keys, err := auth.Load(*apiKeys)
			if err != nil {
				log.Fatalf("load API keys: %v", err) // fail closed: never serve without the keys asked for
			}
			coord.UseKeys(keys)
			go keys.Watch(ctx, 5*time.Second)
		} else {
			log.Print("no API keys file: the public API is open (local use only)")
		}
		handler, metricsHandler = coord.Handler(), coord.MetricsHandler()
		log.Printf("coordinator: %d shards × %d replicas, W=%d R=%d, %d virtual nodes", *expected, *replicas, *w, r, *vnodes)

	case "shard":
		if *shardID == "" || *replicaID == "" || *coordURL == "" || *advertise == "" {
			log.Fatal("shard needs --shard, --replica, --coordinator and --advertise")
		}
		st, err := store.Open(filepath.Join(*dataDir, "shard"))
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		closeStore = st.Close
		rep := shard.NewReplica(st, shard.Config{Shard: *shardID, Replica: *replicaID, W: *w})
		handler, metricsHandler = rep.Handler(), rep.MetricsHandler()
		go rep.Run(ctx)
		go rep.HeartbeatLoop(ctx, *coordURL, *advertise, *interval)
		last, _ := st.Last()
		log.Printf("%s/%s: %d keys replayed up to LSN %d, reporting to %s as %s",
			*shardID, *replicaID, st.Len(), last.LSN, *coordURL, *advertise)

	default:
		log.Fatal("--role must be coordinator or shard")
	}

	api.Set(handler) // logs replayed: serve the real API from now on
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metricsHandler)
	metricsSrv := &http.Server{Addr: *metricsListen, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics listener: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		metricsSrv.Shutdown(shutdown)
		srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s, metrics on %s", *listen, *metricsListen)
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	if err := closeStore(); err != nil {
		log.Printf("close store: %v", err)
	}
}

// parsePairs reads "a=b,c=d" into a map.
func parsePairs(s string) map[string]string {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			m[k] = v
		}
	}
	return m
}
