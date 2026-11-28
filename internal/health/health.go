// Package health serves the endpoints every process has besides its API (M5,
// spec health-endpoints): liveness, readiness and the build version. None needs an API
// key and none carries item data, keys or secrets.
//
//	GET /health/live   200 while the process runs; checks nothing else
//	GET /health/ready  200 when the process can serve its role, else 503 with a reason
//	GET /version       the build compiled into the binary
//
// GET /health keeps its M4 body; it is served by the coordinator and the replica themselves.
package health

import (
	"net/http"
	"runtime"
	"sync/atomic"

	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
	"github.com/iravelmakina/capstone-kv/internal/httpx"
)

// Version is the body of GET /version. Shard and Replica are set only on replicas.
type Version struct {
	Version   string `json:"version"`
	BuildTime string `json:"buildTime"`
	GoVersion string `json:"goVersion"`
	Role      string `json:"role"`
	Shard     string `json:"shard,omitempty"`
	Replica   string `json:"replica,omitempty"`
}

// NewVersion fills Version from the values compiled into the binary, never from
// configuration, so it says what is really running.
func NewVersion(role, shard, replica string) Version {
	return Version{Version: buildinfo.Version, BuildTime: buildinfo.BuildTime,
		GoVersion: runtime.Version(), Role: role, Shard: shard, Replica: replica}
}

// Readiness is the body of GET /health/ready, the same fields for 200 and 503.
type Readiness struct {
	Status string `json:"status"` // ok or unavailable
	Role   string `json:"role"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"` // why not ready
}

// Register adds the three endpoints to mux. ready reports whether the process can serve
// and, if not, why; it is called on every request.
func Register(mux *http.ServeMux, v Version, ready func() (bool, string)) {
	mux.HandleFunc("GET /health/live", live)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		ok, reason := ready()
		writeReady(w, v.Role, ok, reason)
	})
}

func live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeReady(w http.ResponseWriter, role string, ok bool, reason string) {
	body := Readiness{Status: "ok", Role: role, Ready: true}
	code := http.StatusOK
	if !ok {
		body = Readiness{Status: "unavailable", Role: role, Ready: false, Reason: reason}
		code = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, code, body)
}

// Starting is the handler a process serves while it opens and replays its logs, before
// its real handler exists: alive, its version known, not ready ("replaying"), and 503 for
// everything else. So a long replay shows as "live, not ready" instead of a refused
// connection.
func Starting(v Version) http.Handler {
	mux := http.NewServeMux()
	Register(mux, v, func() (bool, string) { return false, "replaying" })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusServiceUnavailable, "starting: replaying the log")
	})
	return mux
}

// Swap is an http.Handler whose target can be replaced while it serves: the listener
// starts with Starting and switches to the real handler once the logs are replayed.
type Swap struct{ h atomic.Pointer[http.Handler] }

// NewSwap returns a Swap serving h.
func NewSwap(h http.Handler) *Swap {
	s := &Swap{}
	s.Set(h)
	return s
}

// Set makes every later request go to h.
func (s *Swap) Set(h http.Handler) { s.h.Store(&h) }

func (s *Swap) ServeHTTP(w http.ResponseWriter, r *http.Request) { (*s.h.Load()).ServeHTTP(w, r) }
