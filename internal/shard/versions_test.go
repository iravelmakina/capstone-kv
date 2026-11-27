package shard

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// send does a request with extra headers and returns the status, the response headers and
// the body.
func send(t *testing.T, method, url, body string, headers ...string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// Spec "Stamp on a timeout": a 504 carries the write's stamp.
func TestStampOnTimeout(t *testing.T) {
	a, b, c := threeNodes(t)
	b.stop()
	c.stop()
	code, h, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"v":1}`)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("put = %d, want 504", code)
	}
	if h.Get(HeaderStamp) == "" {
		t.Fatal("504 has no X-KV-Timestamp")
	}
}

// Spec "Late retry loses": A's put times out with stamp t1; B then writes the key; A
// retries with t1 and is told it was superseded; reads return B's value.
func TestLateRetryLoses(t *testing.T) {
	a, b, c := threeNodes(t)
	c.stop()
	b.stop()
	code, h, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"by":"A"}`)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("A's first put = %d, want 504", code)
	}
	t1 := h.Get(HeaderStamp)
	b = b.restart(t)
	lead(1, a, a, b)
	if code, _, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"by":"B"}`); code != 200 {
		t.Fatalf("B's put = %d", code)
	}
	code, h, _ = send(t, "PUT", a.srv.URL+KeyPath("k"), `{"by":"A"}`, HeaderStamp, t1)
	if code != 200 || h.Get(HeaderSuperseded) != "true" {
		t.Fatalf("A's retry = %d superseded=%q, want 200 and true", code, h.Get(HeaderSuperseded))
	}
	if _, body := do(t, "GET", a.srv.URL+KeyPath("k"), ""); body != `{"by":"B"}` {
		t.Fatalf("read after the retry = %s, want B's value", body)
	}
}

// Spec "Superseded only by a committed write": while the newer write waits for its commit
// and then times out, the older one is answered 504, not "superseded".
func TestSupersededOnlyByCommittedWrite(t *testing.T) {
	a, b, c := threeNodes(t)
	if code := put(t, a, "k", `{"v":0}`); code != 200 {
		t.Fatalf("first put = %d", code)
	}
	_, h, _ := send(t, "GET", a.srv.URL+KeyPath("k"), "")
	old := h.Get(HeaderStamp)
	b.stop()
	c.stop()
	newer := make(chan int, 1)
	go func() {
		code, _, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"v":1}`) // can't commit
		newer <- code
	}()
	time.Sleep(50 * time.Millisecond) // let it reach the log
	code, h, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"v":"retry"}`, HeaderStamp, old)
	if code != http.StatusGatewayTimeout || h.Get(HeaderSuperseded) != "" {
		t.Fatalf("older write = %d superseded=%q, want 504 and no superseded", code, h.Get(HeaderSuperseded))
	}
	if code := <-newer; code != http.StatusGatewayTimeout {
		t.Fatalf("newer write = %d, want 504", code)
	}
}

// Spec "Timestamp in the future": refused with 400, nothing written.
func TestFutureStampRefused(t *testing.T) {
	a, _, _ := threeNodes(t)
	future := uint64(time.Now().Add(60*time.Second).UnixMilli()) << 16
	code, _, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"v":1}`, HeaderStamp, strconv.FormatUint(future, 10))
	if code != http.StatusBadRequest {
		t.Fatalf("put with a stamp 60 s ahead = %d, want 400", code)
	}
	if lastLSN(a) != 0 {
		t.Fatal("a refused write was appended")
	}
	if code, _, _ := send(t, "PUT", a.srv.URL+KeyPath("k"), `{"v":1}`, HeaderStamp, "soon"); code != http.StatusBadRequest {
		t.Fatalf("put with a malformed stamp = %d, want 400", code)
	}
}

// Spec "Lost update prevented" and "Create only", through HTTP; reads carry the ETag.
func TestConditionalWritesOverHTTP(t *testing.T) {
	a, _, _ := threeNodes(t)
	url := a.srv.URL + KeyPath("k")
	if code, _, _ := send(t, "PUT", url, `{"v":1}`, "If-None-Match", "*"); code != 200 {
		t.Fatalf("create = %d", code)
	}
	if code, _, _ := send(t, "PUT", url, `{"v":2}`, "If-None-Match", "*"); code != http.StatusPreconditionFailed {
		t.Fatalf("create over an existing key = %d, want 412", code)
	}
	_, h, _ := send(t, "GET", url, "")
	tag := h.Get("ETag")
	if tag == "" || h.Get(HeaderStamp) == "" {
		t.Fatal("read has no ETag or X-KV-Timestamp")
	}
	if code, _, _ := send(t, "PUT", url, `{"v":3}`, "If-Match", tag); code != 200 {
		t.Fatalf("first update on %s = %d", tag, code)
	}
	if code, _, _ := send(t, "PUT", url, `{"v":4}`, "If-Match", tag); code != http.StatusPreconditionFailed {
		t.Fatalf("second update on the same version = %d, want 412", code)
	}
	if code, _, _ := send(t, "DELETE", url, "", "If-Match", tag); code != http.StatusPreconditionFailed {
		t.Fatalf("delete on an old version = %d, want 412", code)
	}
	if _, body := do(t, "GET", url, ""); body != `{"v":3}` {
		t.Fatalf("value = %s, want the first update's", body)
	}
}
