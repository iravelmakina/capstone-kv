package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Planned leader transfer through the coordinator (ADR-0036, spec leader-placement).

// Spec "Transfer under load" and task 4.2: writers keep putting through the coordinator
// while leadership moves 20 times; every write answered 200 reads back with its value,
// no write gets a 5xx, and each transfer raises the epoch by one.
func TestTransferUnderLoad(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)

	var mu sync.Mutex
	acked := map[string]string{} // key -> value answered 200
	var fivexx []string
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key, val := fmt.Sprintf("w%d-%d", w, i), fmt.Sprintf(`{"n":%d}`, i)
				code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/"+key, val)
				mu.Lock()
				switch {
				case code == 200:
					acked[key] = val
				case code >= 500:
					fivexx = append(fivexx, fmt.Sprintf("%s: %d %s", key, code, body))
				}
				mu.Unlock()
			}
		}(w)
	}

	replicas := []string{"1a", "1b", "1c"}
	epoch := c.members.Status()[0].Epoch
	for i := 0; i < 20; i++ {
		time.Sleep(20 * time.Millisecond) // let writes flow between transfers
		from := c.members.Status()[0].Leader
		to := replicas[(indexOf(replicas, from)+1)%3]
		ans, err := c.members.Transfer(context.Background(), "shard-1", to)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("transfer %d (%s -> %s): %v", i, from, to, err)
		}
		epoch++
		if ans.Leader != to || ans.Epoch != epoch {
			t.Fatalf("transfer %d: leader %s epoch %d, want %s %d", i, ans.Leader, ans.Epoch, to, epoch)
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if len(fivexx) > 0 {
		t.Fatalf("%d writes got a 5xx during transfers, first: %s", len(fivexx), fivexx[0])
	}
	if len(acked) < 50 {
		t.Fatalf("only %d writes acknowledged: the load didn't run", len(acked))
	}
	for key, val := range acked {
		code, body := call(t, "GET", c.coord.URL+"/v1/tables/users/items/"+key, "")
		if code != 200 || body != val {
			t.Fatalf("acknowledged %s=%s reads back %d %q", key, val, code, body)
		}
	}
	t.Logf("%d writes acknowledged across 20 transfers, none lost, no 5xx", len(acked))
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// Spec "Target too slow" through the coordinator: the target is alive by heartbeat but
// can't catch up (its process is gone), so the transfer is abandoned: 409, same leader,
// same epoch, and writes keep working on the old leader.
func TestTransferAbandonedKeepsTheLeader(t *testing.T) {
	c := newCluster(t, 1, true)
	call(t, "POST", c.coord.URL+"/v1/tables", `{"name":"users","partitionKey":"userId"}`)
	before := c.members.Status()[0]
	target := c.others(before.Leader)[0]
	c.nodes[target].stop() // still "live" for the coordinator until its heartbeat times out
	call(t, "PUT", c.coord.URL+"/v1/tables/users/items/u1", `{"v":1}`)

	start := time.Now()
	_, err := c.members.Transfer(context.Background(), "shard-1", target)
	var te *TransferError
	if err == nil || !errors.As(err, &te) || te.Status != 409 {
		t.Fatalf("transfer to a stopped target: %v, want a 409 TransferError", err)
	}
	if took := time.Since(start); took > TransferBudget+500*time.Millisecond {
		t.Fatalf("abandoning took %s, more than the %s budget", took, TransferBudget)
	}
	after := c.members.Status()[0]
	if after.Leader != before.Leader || after.Epoch != before.Epoch {
		t.Fatalf("after abandoning: leader %s epoch %d, want %s %d", after.Leader, after.Epoch, before.Leader, before.Epoch)
	}
	code, body := call(t, "PUT", c.coord.URL+"/v1/tables/users/items/u2", `{"v":2}`)
	expect(t, code, body, 200)
}

// The admin route: 200 with the new epoch, 404 for an unknown replica or shard, 409 for
// the current leader, 400 for a bad body.
func TestTransferAdminRoute(t *testing.T) {
	c := newCluster(t, 1, true)
	leader := c.members.Status()[0].Leader
	to := c.others(leader)[0]
	url := c.coord.URL + "/v1/admin/shards/shard-1/leader"

	code, body := call(t, "POST", url, `{"to":"`+to+`"}`)
	expect(t, code, body, 200)
	var got struct {
		Leader string `json:"leader"`
		Epoch  uint64 `json:"epoch"`
	}
	json.Unmarshal([]byte(body), &got)
	if got.Leader != to || got.Epoch != 2 {
		t.Fatalf("answer %s", body)
	}
	code, body = call(t, "POST", url, `{"to":"`+to+`"}`) // already the leader
	expect(t, code, body, 409)
	code, body = call(t, "POST", url, `{"to":"9z"}`)
	expect(t, code, body, 404)
	code, body = call(t, "POST", c.coord.URL+"/v1/admin/shards/shard-9/leader", `{"to":"1a"}`)
	expect(t, code, body, 404)
	code, body = call(t, "POST", url, `{"leader":"1a"}`)
	expect(t, code, body, 400)
	if !strings.Contains(body, "to") {
		t.Fatalf("400 body: %s", body)
	}
}
