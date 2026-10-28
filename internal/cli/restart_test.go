package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// isolateRouterState points the router state dir at a temp dir so no real daemon is ever seen.
func isolateRouterState(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func decodeRestart(t *testing.T, out string) restartRow {
	t.Helper()
	var doc struct {
		Daemons []restartRow `json:"daemons"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Daemons) != 1 {
		t.Fatalf("json = %q (%v)", out, err)
	}
	return doc.Daemons[0]
}

func TestRestartStoppedRouterIsSkipped(t *testing.T) {
	for _, args := range [][]string{{"--format", "json"}, {"--dry-run", "--format", "json"}} {
		isolateRouterState(t)
		var code int
		out := captureStdout(t, func() { code = cmdRestart(args) })
		if code != 0 {
			t.Fatalf("%v: exit = %d, want 0 (a stopped daemon is not a failure)", args, code)
		}
		if row := decodeRestart(t, out); row.Daemon != "proxy" || row.Status != "skipped" || row.OldPID != 0 || row.NewPID != 0 {
			t.Errorf("%v: row = %+v", args, row)
		}
		if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); err == nil {
			t.Errorf("%v: a skipped restart created router.json", args)
		}
	}
}

func TestRestartDryRunRunningRouterChangesNothing(t *testing.T) {
	isolateRouterState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	state := filepath.Join(paths.StateDir(), "router.json")
	doc := fmt.Sprintf(`{"pid":%d,"port":%d}`, os.Getpid(), port)
	if err := os.WriteFile(state, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { code = cmdRestart([]string{"--dry-run", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if row := decodeRestart(t, out); row.Status != "would restart" || row.OldPID != os.Getpid() || row.NewPID != 0 {
		t.Errorf("row = %+v", row)
	}
	if b, _ := os.ReadFile(state); string(b) != doc {
		t.Errorf("dry-run rewrote router.json: %q", b)
	}
}

func TestRestartRejectsBadFormat(t *testing.T) {
	isolateRouterState(t)
	if code := cmdRestart([]string{"--format", "yaml"}); code == 0 {
		t.Fatal("bad --format accepted")
	}
}
