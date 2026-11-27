// Package capstonekv is the go-ycsb binding for capstone-kv (ADR-0016): it turns YCSB's
// operations into calls to the coordinator's REST API (api/openapi.yaml).
//
//	YCSB Insert / Update -> PUT    /v1/tables/{table}/items/{key}   (whole record)
//	YCSB Read            -> GET    /v1/tables/{table}/items/{key}
//	YCSB Delete          -> DELETE /v1/tables/{table}/items/{key}
//	YCSB Scan            -> not supported: M2 has no range queries (ADR-0012)
//
// A YCSB record is up to 10 fields of 100 random letters; it is stored as one JSON object
// {"field0": "...", ...}. Our PUT replaces the whole item, so benchmarks run with
// writeallfields=true: every update writes the full record, which is what PUT means.
package capstonekv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// Properties this binding reads (set with -p name=value).
const (
	propURL     = "kv.url"     // coordinator base URL
	propTimeout = "kv.timeout" // per-request timeout
	propAPIKey  = "kv.apikey"  // API key (M4, ADR-0027); empty for a store without keys
)

func init() { ycsb.RegisterDBCreator("capstonekv", creator{}) }

type creator struct{}

// Create connects to the coordinator, waits until it has all its shards, and registers
// the benchmark table (partition key only; a second run finds it already there).
func (creator) Create(p *properties.Properties) (ycsb.DB, error) {
	threads := p.GetInt(prop.ThreadCount, int(prop.ThreadCountDefault)) // go-ycsb's default is 200: set it
	db := &kvDB{
		base:   strings.TrimRight(p.GetString(propURL, "http://localhost:8080"), "/"),
		apiKey: p.GetString(propAPIKey, ""),
		client: &http.Client{
			Timeout: p.GetParsedDuration(propTimeout, 10*time.Second),
			// One kept-alive connection per benchmark thread (and some slack), so the
			// numbers measure the store, not TCP handshakes.
			Transport: &http.Transport{MaxIdleConns: 2 * threads, MaxIdleConnsPerHost: 2 * threads},
		},
	}
	if err := db.waitReady(60 * time.Second); err != nil {
		return nil, err
	}
	table := p.GetString(prop.TableName, prop.TableNameDefault)
	if err := db.ensureTable(table); err != nil {
		return nil, err
	}
	return db, nil
}

type kvDB struct {
	base   string
	apiKey string
	client *http.Client
}

// authorize adds the API key to a request, if one is set.
func (db *kvDB) authorize(req *http.Request) {
	if db.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+db.apiKey)
	}
}

// waitReady polls /health until the coordinator reports "ready" (all shards registered).
func (db *kvDB) waitReady(limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		resp, err := db.client.Get(db.base + "/health")
		if err == nil {
			var h struct {
				Ready bool `json:"ready"`
			}
			json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if h.Ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("coordinator at %s not ready after %s", db.base, limit)
		}
		time.Sleep(time.Second)
	}
}

func (db *kvDB) ensureTable(table string) error {
	body := fmt.Sprintf(`{"name":%q,"partitionKey":"ycsb_key"}`, table)
	req, _ := http.NewRequest(http.MethodPost, db.base+"/v1/tables", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	db.authorize(req)
	resp, err := db.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("create table %s: status %d", table, resp.StatusCode)
	}
	return nil
}

func (db *kvDB) itemURL(table, key string) string {
	return db.base + "/v1/tables/" + url.PathEscape(table) + "/items/" + url.PathEscape(key)
}

// do sends one request and checks the status; the body is drained so the connection is reused.
func (db *kvDB) do(ctx context.Context, method, u string, body []byte, want int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	db.authorize(req)
	resp, err := db.client.Do(req)
	if err != nil {
		return nil, logError(err)
	}
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		drain(resp)
		return nil, logError(fmt.Errorf("%s %s: status %d %s", method, u, resp.StatusCode, strings.TrimSpace(string(msg))))
	}
	return resp, nil
}

// errorsLogged counts failed requests; the first few are printed with their cause, since
// go-ycsb's summary only counts them (READ_ERROR, UPDATE_ERROR).
var errorsLogged atomic.Int64

func logError(err error) error {
	if n := errorsLogged.Add(1); n <= 10 {
		log.Printf("request failed (%d): %v", n, err)
	}
	return err
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// put writes a whole record as one JSON object of string fields.
func (db *kvDB) put(ctx context.Context, table, key string, values map[string][]byte) error {
	obj := make(map[string]string, len(values))
	for f, v := range values {
		obj[f] = string(v) // YCSB values are ASCII letters
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	resp, err := db.do(ctx, http.MethodPut, db.itemURL(table, key), body, http.StatusOK)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

func (db *kvDB) Insert(ctx context.Context, table, key string, values map[string][]byte) error {
	return db.put(ctx, table, key, values)
}

func (db *kvDB) Update(ctx context.Context, table, key string, values map[string][]byte) error {
	return db.put(ctx, table, key, values)
}

func (db *kvDB) Read(ctx context.Context, table, key string, fields []string) (map[string][]byte, error) {
	resp, err := db.do(ctx, http.MethodGet, db.itemURL(table, key), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	var obj map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(obj))
	if len(fields) == 0 { // all fields
		for f, v := range obj {
			out[f] = []byte(v)
		}
		return out, nil
	}
	for _, f := range fields {
		if v, ok := obj[f]; ok {
			out[f] = []byte(v)
		}
	}
	return out, nil
}

func (db *kvDB) Delete(ctx context.Context, table, key string) error {
	resp, err := db.do(ctx, http.MethodDelete, db.itemURL(table, key), nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

var errScan = errors.New("scan is not supported: M2 has no range queries (ADR-0012)")

func (db *kvDB) Scan(context.Context, string, string, int, []string) ([]map[string][]byte, error) {
	return nil, errScan
}

func (db *kvDB) InitThread(ctx context.Context, _, _ int) context.Context { return ctx }
func (db *kvDB) CleanupThread(context.Context)                            {}

func (db *kvDB) Close() error {
	db.client.CloseIdleConnections()
	return nil
}
