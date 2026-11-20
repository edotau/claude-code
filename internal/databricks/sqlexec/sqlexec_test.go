package sqlexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const (
	succeeded = `{"statement_id":"s1","status":{"state":"SUCCEEDED"},"manifest":{"schema":{"columns":[{"name":"model"},{"name":"usd"}]}},`
	oneRow    = `"result":{"chunk_index":0,"data_array":[["m1","1.5"]]}}`
)

// fake is a TLS workspace that answers each request path from replies, in order, and records what it saw.
type fake struct {
	mu      sync.Mutex
	replies map[string][]string
	seen    []string
	auth    []string
	post    map[string]any
	sleeps  []time.Duration
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.seen, f.auth = append(f.seen, key), append(f.auth, r.Header.Get("Authorization"))
	if key == "POST "+statementsPath {
		_ = json.NewDecoder(r.Body).Decode(&f.post)
	}
	queue := f.replies[key]
	if len(queue) == 0 {
		http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
		return
	}
	reply := queue[0]
	f.replies[key] = queue[1:]
	if after, ok := strings.CutPrefix(reply, "429 "); ok {
		w.Header().Set("Retry-After", after)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	_, _ = w.Write([]byte(reply))
}

// run serves replies, swaps the client and the sleep for the fake, and calls Run.
func run(t *testing.T, replies map[string][]string) (*fake, *httptest.Server, Result, error) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("SQLEXEC_TEST_TOKEN", "synthetic-token")
	f := &fake{replies: replies}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	oldClient, oldSleep := httpClient, textutil.RetrySleep
	httpClient = srv.Client
	textutil.RetrySleep = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.sleeps = append(f.sleeps, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { httpClient, textutil.RetrySleep = oldClient, oldSleep })
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Host: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "SQLEXEC_TEST_TOKEN"}}
	res, err := Run(context.Background(), p, "wh", "SELECT 1")
	return f, srv, res, err
}

func TestRunInlineSuccess(t *testing.T) {
	f, _, res, err := run(t, map[string][]string{"POST " + statementsPath: {succeeded + oneRow}})
	want := Result{Columns: []string{"model", "usd"}, Rows: [][]string{{"m1", "1.5"}}}
	if err != nil || !reflect.DeepEqual(res, want) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if f.post["disposition"] != "INLINE" || f.post["format"] != "JSON_ARRAY" || f.post["row_limit"] != float64(rowLimit) ||
		f.post["on_wait_timeout"] != "CONTINUE" || f.post["warehouse_id"] != "wh" {
		t.Fatalf("post body %v", f.post)
	}
	if f.auth[0] != "Bearer synthetic-token" {
		t.Fatalf("auth header not set (%d bytes)", len(f.auth[0]))
	}
}

func TestRunPendingThenSuccess(t *testing.T) {
	f, _, res, err := run(t, map[string][]string{
		"POST " + statementsPath:        {`{"statement_id":"s1","status":{"state":"PENDING"}}`},
		"GET " + statementsPath + "/s1": {`{"statement_id":"s1","status":{"state":"RUNNING"}}`, succeeded + oneRow},
	})
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if !reflect.DeepEqual(f.sleeps, []time.Duration{pollEvery, pollEvery}) {
		t.Fatalf("poll waits %v", f.sleeps)
	}
}

func TestRunFailedCarriesMessageNotHost(t *testing.T) {
	_, srv, _, err := run(t, map[string][]string{
		"POST " + statementsPath: {`{"statement_id":"s1","status":{"state":"FAILED","error":{"message":"TABLE_OR_VIEW_NOT_FOUND"}}}`},
	})
	u, _ := url.Parse(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "failed: TABLE_OR_VIEW_NOT_FOUND") || strings.Contains(err.Error(), u.Hostname()) {
		t.Fatalf("err %v", err)
	}
}

func TestRunFollowsSecondChunk(t *testing.T) {
	first := strings.Replace(oneRow, `]]}}`, `]],"next_chunk_index":1}}`, 1)
	f, _, res, err := run(t, map[string][]string{
		"POST " + statementsPath:                        {succeeded + first},
		"GET " + statementsPath + "/s1/result/chunks/1": {`{"chunk_index":1,"data_array":[["m2",null]]}`},
	})
	if err != nil || !reflect.DeepEqual(res.Rows, [][]string{{"m1", "1.5"}, {"m2", ""}}) || res.Truncated {
		t.Fatalf("got %+v, %v (seen %v)", res, err, f.seen)
	}
}

func TestRunRetries429WithRetryAfter(t *testing.T) {
	f, _, res, err := run(t, map[string][]string{"POST " + statementsPath: {"429 3", succeeded + oneRow}})
	if err != nil || len(res.Rows) != 1 || len(f.seen) != 2 {
		t.Fatalf("got %+v, %v (seen %v)", res, err, f.seen)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] < 3*time.Second {
		t.Fatalf("retry waits %v, want one of at least 3s", f.sleeps)
	}
}
