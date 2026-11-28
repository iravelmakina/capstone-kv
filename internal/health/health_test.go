package health

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/iravelmakina/capstone-kv/internal/buildinfo"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

// Spec "Build version": the values come from the binary (buildinfo), not configuration.
func TestVersionFromTheBinary(t *testing.T) {
	oldV, oldT := buildinfo.Version, buildinfo.BuildTime
	t.Cleanup(func() { buildinfo.Version, buildinfo.BuildTime = oldV, oldT })
	buildinfo.Version, buildinfo.BuildTime = "abc123def456", "2026-11-28T10:00:00Z"

	mux := http.NewServeMux()
	Register(mux, NewVersion("shard", "shard-1", "1a"), func() (bool, string) { return true, "" })
	code, body := get(t, mux, "/version")
	if code != 200 {
		t.Fatalf("/version = %d", code)
	}
	var v Version
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	want := Version{Version: "abc123def456", BuildTime: "2026-11-28T10:00:00Z", GoVersion: runtime.Version(),
		Role: "shard", Shard: "shard-1", Replica: "1a"}
	if v != want {
		t.Fatalf("/version = %+v, want %+v", v, want)
	}
}

// The coordinator's /version has no shard or replica fields at all.
func TestCoordinatorVersionOmitsShardFields(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, NewVersion("coordinator", "", ""), func() (bool, string) { return true, "" })
	_, body := get(t, mux, "/version")
	if strings.Contains(body, "shard") || strings.Contains(body, "replica") {
		t.Fatalf("coordinator /version has shard fields: %s", body)
	}
}

// Spec "Readiness": 200 when ready, 503 with the same fields and a reason when not.
func TestReadyAndNotReady(t *testing.T) {
	ready, reason := false, "2 of 3 shards registered"
	mux := http.NewServeMux()
	Register(mux, NewVersion("coordinator", "", ""), func() (bool, string) { return ready, reason })

	code, body := get(t, mux, "/health/ready")
	var r Readiness
	json.Unmarshal([]byte(body), &r)
	if code != 503 || r != (Readiness{Status: "unavailable", Role: "coordinator", Ready: false, Reason: reason}) {
		t.Fatalf("not ready: %d %s", code, body)
	}
	if code, _ := get(t, mux, "/health/live"); code != 200 {
		t.Fatalf("live while not ready = %d, want 200", code)
	}

	ready, reason = true, ""
	code, body = get(t, mux, "/health/ready")
	r = Readiness{}
	json.Unmarshal([]byte(body), &r)
	if code != 200 || r != (Readiness{Status: "ok", Role: "coordinator", Ready: true}) {
		t.Fatalf("ready: %d %s", code, body)
	}
}

// Spec "Replica replaying its log": while the log replays the node is live and not ready
// ("replaying"), and every other route answers 503; after Set, the real handler serves.
func TestStartingThenSwapped(t *testing.T) {
	s := NewSwap(Starting(NewVersion("shard", "shard-1", "1a")))

	if code, _ := get(t, s, "/health/live"); code != 200 {
		t.Fatalf("live while replaying = %d, want 200", code)
	}
	code, body := get(t, s, "/health/ready")
	if code != 503 || !strings.Contains(body, `"reason":"replaying"`) {
		t.Fatalf("ready while replaying = %d %s", code, body)
	}
	if code, _ := get(t, s, "/version"); code != 200 {
		t.Fatalf("version while replaying = %d, want 200", code)
	}
	if code, _ := get(t, s, "/internal/v1/kv/x"); code != 503 {
		t.Fatalf("item route while replaying = %d, want 503", code)
	}

	real := http.NewServeMux()
	real.HandleFunc("GET /internal/v1/kv/{key}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	s.Set(real)
	if code, _ := get(t, s, "/internal/v1/kv/x"); code != 404 {
		t.Fatalf("after Set = %d, want the real handler's 404", code)
	}
}
