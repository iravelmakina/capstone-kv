package coordinator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iravelmakina/capstone-kv/internal/ring"
	"github.com/iravelmakina/capstone-kv/internal/shard"
	"github.com/iravelmakina/capstone-kv/internal/store"
)

// Leader placement (ADR-0036, spec leader-placement). Replicas are named by address on
// three nodes, two replicas per node as in AWS: node A 10.0.0.1, B 10.0.0.2, C 10.0.0.3,
// shard-1 on port 8081 and shard-2 on 8082.

var nodeIP = map[byte]string{'a': "10.0.0.1", 'b': "10.0.0.2", 'c': "10.0.0.3"}

func addrOf(replica string) string { // "1a" -> http://10.0.0.1:8081
	return "http://" + nodeIP[replica[1]] + ":808" + replica[:1]
}

type placement struct {
	t   *testing.T
	m   *Membership
	now time.Time
}

func newPlacement(t *testing.T, preferred map[string]string) *placement {
	t.Helper()
	m, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: 2, Replicas: 3, Leaders: openStore(t), Preferred: preferred,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &placement{t: t, m: m, now: time.Now()}
	m.now = func() time.Time { return p.now }
	return p
}

// beat sends one heartbeat per replica, all with the same last record (equally caught up).
func (p *placement) beat(replicas ...string) {
	p.t.Helper()
	for _, r := range replicas {
		s := p.m.Status()
		var epoch uint64
		for _, st := range s {
			if st.ID == "shard-"+r[:1] {
				epoch = st.Epoch
			}
		}
		if _, err := p.m.Heartbeat(shard.Heartbeat{Shard: "shard-" + r[:1], Replica: r, Address: addrOf(r),
			Epoch: epoch, Last: store.Version{Epoch: 1, LSN: 10}}); err != nil {
			p.t.Fatal(err)
		}
	}
}

func (p *placement) leader(shardID string) string {
	for _, s := range p.m.Status() {
		if s.ID == shardID {
			return s.Leader
		}
	}
	return ""
}

// Spec "Late node at start-up": node a registers two minutes after b and c. Before
// ADR-0036 both leaders went to node b after one heartbeat timeout (M4 sessions 1 and 3);
// now the sets wait, and the preferred leaders 1a and 2b land on different nodes.
func TestLateNodeWithinTheWait(t *testing.T) {
	p := newPlacement(t, map[string]string{"shard-1": "1a", "shard-2": "2b"})
	p.beat("1b", "1c", "2b", "2c")
	p.now = p.now.Add(20 * time.Second)
	p.beat("1b", "1c", "2b", "2c")
	if l1, l2 := p.leader("shard-1"), p.leader("shard-2"); l1 != "" || l2 != "" {
		t.Fatalf("leaders chosen before node a: %q %q", l1, l2)
	}
	p.now = p.now.Add(100 * time.Second) // two minutes in all
	p.beat("1b", "1c", "2b", "2c", "1a", "2a")
	if l1, l2 := p.leader("shard-1"), p.leader("shard-2"); l1 != "1a" || l2 != "2b" {
		t.Fatalf("leaders %q, %q; want 1a and 2b", l1, l2)
	}
	if perNode, _ := p.m.LeaderPlacement(); perNode["10.0.0.1"] != 1 || perNode["10.0.0.2"] != 1 || perNode["10.0.0.3"] != 0 {
		t.Fatalf("leaders per node = %v", perNode)
	}
}

// Spec "Placement tie-break" at first assignment: node a never comes. After the wait,
// shard-2 takes its preferred 2b (node b); shard-1 has 1b and 1c equally caught up and
// takes 1c, on the node without a leader (before: 1b, the lowest ID, on node b).
func TestTieBreakAtFirstAssignment(t *testing.T) {
	p := newPlacement(t, map[string]string{"shard-1": "1a", "shard-2": "2b"})
	p.beat("2b", "2c")
	p.now = p.now.Add(3*time.Minute + time.Second)
	p.beat("2b", "2c") // shard-2 first: 2b
	p.beat("1b", "1c")
	p.now = p.now.Add(3*time.Minute + time.Second)
	p.beat("2b", "2c", "1b", "1c")
	if l1, l2 := p.leader("shard-1"), p.leader("shard-2"); l1 != "1c" || l2 != "2b" {
		t.Fatalf("leaders %q, %q; want 1c and 2b", l1, l2)
	}
}

// fakeReplicas answers the coordinator's GET /health on each replica's address with the
// given last record, as askLast reads it before a promotion.
type fakeReplicas map[string]store.Version // address -> last

func (f fakeReplicas) RoundTrip(req *http.Request) (*http.Response, error) {
	last, ok := f["http://"+req.URL.Host]
	if !ok {
		return nil, io.EOF
	}
	b, _ := json.Marshal(shard.Status{Status: "ok", Last: last})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: http.Header{}}, nil
}

// Spec "Equally caught-up followers" and "Safety first" at promotion: shard-1's leader on
// node a dies; 1b sits on node b, which leads shard-2, 1c on node c, which leads nothing.
func TestTieBreakAtPromotion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		b, c     store.Version // what 1b and 1c report
		want     string
		whyLabel string
	}{
		{"equal: the free node wins", store.Version{Epoch: 1, LSN: 10}, store.Version{Epoch: 1, LSN: 10}, "1c", "tie-break"},
		{"newer on the busy node: safety wins", store.Version{Epoch: 1, LSN: 11}, store.Version{Epoch: 1, LSN: 10}, "1b", "safety"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlacement(t, map[string]string{"shard-1": "1a", "shard-2": "2b"})
			p.beat("1a", "1b", "1c", "2a", "2b", "2c")
			if p.leader("shard-1") != "1a" || p.leader("shard-2") != "2b" {
				t.Fatalf("setup: leaders %q %q", p.leader("shard-1"), p.leader("shard-2"))
			}
			p.now = p.now.Add(20 * time.Second) // 1a silent
			p.beat("1b", "1c", "2a", "2b", "2c")
			p.m.client = &http.Client{Transport: fakeReplicas{addrOf("1b"): tc.b, addrOf("1c"): tc.c}}
			p.m.CheckLeaders(context.Background())
			if got := p.leader("shard-1"); got != tc.want {
				t.Fatalf("promoted %s, want %s (%s)", got, tc.want, tc.whyLabel)
			}
		})
	}
}

// Spec "Colocated leaders reported": both leaders on node b, as the coordinator persisted
// them (M4 session 2: after a lab restart, both leaders sat on one node), every replica
// live; kv_node_leaders and kv_leaders_colocated show it. The "preferred" setting alone
// can't produce this any more: a leaderless node outranks the preferred replica.
func TestColocatedLeadersReported(t *testing.T) {
	leaders := openStore(t)
	for id, r := range map[string]string{"shard-1": "1b", "shard-2": "2b"} {
		raw, _ := json.Marshal(assignment{Leader: r, Epoch: 1})
		if err := leaders.Put(id, raw); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewMembership(ring.New(ring.DefaultVirtualNodes), MembershipConfig{
		Timeout: 15 * time.Second, ExpectedShards: 2, Replicas: 3, Leaders: leaders,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &placement{t: t, m: m, now: time.Now()}
	m.now = func() time.Time { return p.now }
	p.beat("1a", "1b", "1c", "2a", "2b", "2c")
	perNode, colocated := p.m.LeaderPlacement()
	if perNode["10.0.0.2"] != 2 || perNode["10.0.0.1"] != 0 || perNode["10.0.0.3"] != 0 {
		t.Fatalf("leaders per node = %v", perNode)
	}
	if strings.Join(colocated, ",") != "shard-1,shard-2" {
		t.Fatalf("colocated = %v, want both (each has a follower on a leaderless node)", colocated)
	}
	metrics := scrapeMetrics(t, newCoordMetrics(p.m))
	for _, want := range []string{`kv_node_leaders{node="10.0.0.2"} 2`, `kv_node_leaders{node="10.0.0.1"} 0`, "kv_leaders_colocated 2"} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics lack %q", want)
		}
	}

	// Nodes a and c silent: no follower could take a leader, so nothing is reported as
	// colocated (the node alerts cover that state).
	p.now = p.now.Add(20 * time.Second)
	p.beat("1b", "2b")
	if _, colocated := p.m.LeaderPlacement(); len(colocated) != 0 {
		t.Fatalf("colocated with no live follower elsewhere = %v", colocated)
	}
}

func scrapeMetrics(t *testing.T, cm *coordMetrics) string {
	t.Helper()
	var b strings.Builder
	cm.reg.Write(&b)
	return b.String()
}
